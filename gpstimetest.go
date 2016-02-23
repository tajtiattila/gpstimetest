package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"camlistore.org/third_party/github.com/bradfitz/latlong"
	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
)

func main() {
	flag.Parse()

	for _, root := range flag.Args() {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}

			rel, _ := filepath.Rel(root, path)

			f, err := os.Open(path)
			if err != nil {
				fmt.Printf("%s open: %v\n", rel, err)
				return nil
			}
			defer f.Close()

			et, err := findExifTimes(f)
			if err != nil {
				fmt.Printf("%s exif: %v\n", rel, err)
				return nil
			}

			fmt.Printf("%s: %v\n", rel, et.String())
			return nil
		})
	}
}

type exifTimes struct {
	Model string

	DateTime    time.Time
	Corrected   time.Time
	GPSDateTime time.Time

	HasGPSLoc bool
}

func (t exifTimes) String() string {
	dt := !t.DateTime.IsZero()
	ct := !t.Corrected.IsZero() // implies dt
	gt := !t.GPSDateTime.IsZero()
	switch {
	case !dt && !gt:
		return "! all times missing"
	case !dt && gt:
		return fmt.Sprintf("! %v only GPSDateTime=%v", t.Model, t.GPSDateTime)
	case !ct && gt:
		var msg string
		if t.HasGPSLoc {
			msg = "lat/long correction failed"
		} else {
			msg = "no GPS location"
		}
		return fmt.Sprintf("! %v %s GPSDateTime=%v", t.Model, msg, t.GPSDateTime)
	case ct && gt:
		return fmt.Sprintf("%v Delta=%v, GPSDateTime=%v", t.Model, t.Corrected.Sub(t.GPSDateTime), t.GPSDateTime)
	default:
		return "! impossible"
	}
}

func findExifTimes(r io.Reader) (exifTimes, error) {
	var ret exifTimes

	ex, err := exif.Decode(r)
	if err != nil {
		if exif.IsCriticalError(err) || exif.IsExifError(err) {
			return ret, err
		}
	}

	if mt, err := ex.Get(exif.FieldName("Model")); err == nil {
		ret.Model = mt.String()
	}

	gt, err := exifGPSDateTime(ex)
	if err == nil {
		ret.GPSDateTime = gt
	}

	ret.DateTime, err = ex.DateTime()
	if err != nil {
		if !ret.GPSDateTime.IsZero() {
			return ret, nil
		}
		return ret, err
	}

	if ret.DateTime.Location() == time.Local {
		if lat, long, err := ex.LatLong(); err == nil {
			ret.HasGPSLoc = true
			if loc := lookupLocation(latlong.LookupZoneName(lat, long)); loc != nil {
				if t, err := exifDateTimeInLocation(ex, loc); err == nil {
					ret.Corrected = t
				}
			}
		}
	}
	return ret, nil
}

func exifDateTime(x *exif.Exif) (time.Time, error) {
	dt, err := exifGPSDateTime(x)
	if err == nil {
		return dt, err
	}
	return x.DateTime()
}

// exigGPSDateTime retrieves get GPS date/time. It is always UTC.
// see EXIF 2.3 SPEC: http://www.cipa.jp/std/documents/e/DC-008-2012_E.pdf
func exifGPSDateTime(x *exif.Exif) (time.Time, error) {
	dateTag, err := x.Get(exif.FieldName("GPSDateStamp"))
	if err != nil {
		return time.Time{}, err
	}
	timeTag, err := x.Get(exif.FieldName("GPSTimeStamp"))
	if err != nil {
		return time.Time{}, err
	}

	dateVal, err := dateTag.StringVal()
	if err != nil {
		return time.Time{}, err
	}
	date, err := time.Parse("2006:01:02", dateVal)
	if err != nil {
		return time.Time{}, err
	}

	tnano := func(i int, unit time.Duration) time.Duration {
		if err != nil {
			return 0
		}
		var num, denom int64
		num, denom, err = timeTag.Rat2(i)
		if err != nil {
			return 0
		}
		if denom == 0 {
			err = errors.New("EXIF GPSTimeStamp: zero denominator")
			return 0
		}
		return time.Duration(num) * unit / time.Duration(denom)
	}
	nanos := tnano(0, time.Hour) + tnano(1, time.Minute) + tnano(2, time.Second)
	if err != nil {
		return time.Time{}, err
	}

	return date.Add(nanos), nil
}

// This is basically a copy of the exif.Exif.DateTime() method, except:
//   * it takes a *time.Location to assume
//   * the caller already assumes there's no timezone offset or GPS time
//     in the EXIF, so any of that code can be ignored.
func exifDateTimeInLocation(x *exif.Exif, loc *time.Location) (time.Time, error) {
	tag, err := x.Get(exif.DateTimeOriginal)
	if err != nil {
		tag, err = x.Get(exif.DateTime)
		if err != nil {
			return time.Time{}, err
		}
	}
	if tag.Format() != tiff.StringVal {
		return time.Time{}, errors.New("DateTime[Original] not in string format")
	}
	const exifTimeLayout = "2006:01:02 15:04:05"
	dateStr := strings.TrimRight(string(tag.Val), "\x00")
	return time.ParseInLocation(exifTimeLayout, dateStr, loc)
}

var zoneCache struct {
	sync.RWMutex
	m map[string]*time.Location
}

func lookupLocation(zone string) *time.Location {
	if zone == "" {
		return nil
	}
	zoneCache.RLock()
	l, ok := zoneCache.m[zone]
	zoneCache.RUnlock()
	if ok {
		return l
	}
	// could use singleflight here, but doesn't really
	// matter if two callers both do this.
	loc, err := time.LoadLocation(zone)

	zoneCache.Lock()
	if zoneCache.m == nil {
		zoneCache.m = make(map[string]*time.Location)
	}
	zoneCache.m[zone] = loc // even if nil
	zoneCache.Unlock()

	if err != nil {
		log.Printf("failed to lookup timezone %q: %v", zone, err)
		return nil
	}
	return loc
}
