package subtitle

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func parseClock(value string, allowShort bool, fractionSeparators string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	sep := strings.LastIndexAny(value, fractionSeparators)
	if sep < 0 {
		return 0, fmt.Errorf("timestamp %q has no fractional separator", value)
	}
	whole, fraction := value[:sep], value[sep+1:]
	if len(fraction) < 1 || len(fraction) > 3 || !asciiDigits(fraction) {
		return 0, fmt.Errorf("invalid timestamp fraction in %q", value)
	}
	for len(fraction) < 3 {
		fraction += "0"
	}
	parts := strings.Split(whole, ":")
	if len(parts) != 3 && !(allowShort && len(parts) == 2) {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	var hourText, minuteText, secondText string
	if len(parts) == 3 {
		hourText, minuteText, secondText = parts[0], parts[1], parts[2]
	} else {
		hourText, minuteText, secondText = "0", parts[0], parts[1]
	}
	if !asciiDigits(hourText) || !asciiDigits(minuteText) || !asciiDigits(secondText) {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	if len(minuteText) != 2 || len(secondText) != 2 {
		return 0, fmt.Errorf("invalid timestamp field width in %q", value)
	}
	hours, err := strconv.ParseUint(hourText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp hours in %q", value)
	}
	minutes, _ := strconv.ParseUint(minuteText, 10, 64)
	seconds, _ := strconv.ParseUint(secondText, 10, 64)
	millis, _ := strconv.ParseUint(fraction, 10, 64)
	if minutes > 59 || seconds > 59 {
		return 0, fmt.Errorf("timestamp field out of range in %q", value)
	}
	// Keep the sum in range for time.Duration rather than allowing a large
	// syntactically valid timestamp to wrap negative.
	totalSeconds := hours
	if totalSeconds > math.MaxInt64/60 {
		return 0, fmt.Errorf("timestamp %q is too large", value)
	}
	totalSeconds = totalSeconds*60 + minutes
	if totalSeconds > math.MaxInt64/60 {
		return 0, fmt.Errorf("timestamp %q is too large", value)
	}
	totalSeconds = totalSeconds*60 + seconds
	limitMillis := uint64(math.MaxInt64) / uint64(time.Millisecond)
	if totalSeconds > limitMillis/1000 {
		return 0, fmt.Errorf("timestamp %q is too large", value)
	}
	totalMillis := totalSeconds*1000 + millis
	if totalMillis > limitMillis {
		return 0, fmt.Errorf("timestamp %q is too large", value)
	}
	return time.Duration(totalMillis) * time.Millisecond, nil
}

func parseASSTime(value string) (time.Duration, error) {
	// ASS conventionally uses H:MM:SS.cc. Permit one through three fractional
	// digits so source produced by common tools is not rejected unnecessarily.
	return parseClock(value, false, ".")
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
