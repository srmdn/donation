package app

import (
	"fmt"
	"strings"
	"time"
)

const deadlineLayout = "2006-01-02"

func NormalizeDeadlineDate(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if _, err := time.ParseInLocation(deadlineLayout, value, jakartaLocation()); err != nil {
		return "", fmt.Errorf("deadline must use YYYY-MM-DD")
	}
	return value, nil
}

func DeadlineStatus(deadlineDate string, now time.Time) (string, bool) {
	if strings.TrimSpace(deadlineDate) == "" {
		return "", false
	}

	deadline, err := time.ParseInLocation(deadlineLayout, deadlineDate, jakartaLocation())
	if err != nil {
		return "", false
	}

	localNow := now.In(jakartaLocation())
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, jakartaLocation())
	deadlineDay := time.Date(deadline.Year(), deadline.Month(), deadline.Day(), 0, 0, 0, 0, jakartaLocation())
	daysLeft := int(deadlineDay.Sub(today).Hours() / 24)

	switch {
	case daysLeft < 0:
		return "Pendanaan berakhir", true
	case daysLeft == 0:
		return "Berakhir hari ini", false
	default:
		return fmt.Sprintf("Sisa %d hari", daysLeft), false
	}
}

func jakartaLocation() *time.Location {
	location, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return time.FixedZone("WIB", 7*60*60)
	}
	return location
}
