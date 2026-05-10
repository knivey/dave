package main

import (
	"fmt"
	"strconv"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

var loggerBans = logxi.New("bans")

func init() {
	loggerBans.SetLevel(logxi.LevelAll)
}

func isBanned(db *gorm.DB, userID int64, network, channel, service string) bool {
	if db == nil {
		return false
	}
	now := time.Now()
	var bans []Ban
	db.Where("user_id = ? AND active = ? AND network = ?", userID, true, network).Find(&bans)
	for i := range bans {
		if !bans[i].ExpiresAt.IsZero() && bans[i].ExpiresAt.Before(now) {
			deactivateBan(db, bans[i].ID)
			continue
		}
		if bans[i].Channel != "" && bans[i].Channel != channel {
			continue
		}
		if bans[i].ServiceScope != "" && bans[i].ServiceScope != service {
			continue
		}
		return true
	}
	return false
}

func createBan(db *gorm.DB, userID int64, network, channel, serviceScope, reason string, duration time.Duration, bannerUserID *int64, bannerNick string) (*Ban, error) {
	expiresAt := time.Now().Add(duration)
	ban := Ban{
		UserID:       userID,
		Network:      network,
		Channel:      channel,
		ServiceScope: serviceScope,
		Reason:       reason,
		Duration:     duration,
		ExpiresAt:    expiresAt,
		Active:       true,
		BannerUserID: bannerUserID,
		BannerNick:   bannerNick,
		CreatedAt:    time.Now(),
	}
	if err := db.Create(&ban).Error; err != nil {
		return nil, err
	}
	loggerBans.Info("ban created", "ban_id", ban.ID, "user_id", userID, "network", network, "channel", channel, "reason", reason, "duration", formatDuration(duration))
	return &ban, nil
}

func deactivateBan(db *gorm.DB, banID int64) error {
	now := time.Now()
	return db.Model(&Ban{}).Where("id = ? AND active = ?", banID, true).
		Updates(map[string]interface{}{"active": false, "deactivated_at": &now}).Error
}

func deactivateBansForUser(db *gorm.DB, userID int64, network string) error {
	now := time.Now()
	return db.Model(&Ban{}).Where("user_id = ? AND network = ? AND active = ?", userID, network, true).
		Updates(map[string]interface{}{"active": false, "deactivated_at": &now}).Error
}

func getActiveBansForUser(db *gorm.DB, userID int64, network string) []Ban {
	if db == nil {
		return nil
	}
	now := time.Now()
	var bans []Ban
	db.Where("user_id = ? AND active = ? AND network = ?", userID, true, network).Order("created_at DESC").Find(&bans)
	var active []Ban
	for i := range bans {
		if !bans[i].ExpiresAt.IsZero() && bans[i].ExpiresAt.Before(now) {
			deactivateBan(db, bans[i].ID)
			continue
		}
		active = append(active, bans[i])
	}
	return active
}

func getActiveBans(db *gorm.DB, network string) ([]Ban, error) {
	var bans []Ban
	err := db.Where("network = ? AND active = ?", network, true).Order("created_at DESC").Find(&bans).Error
	return bans, err
}

func getBanHistory(db *gorm.DB, userID int64) ([]Ban, error) {
	var bans []Ban
	err := db.Where("user_id = ?", userID).Order("created_at DESC").Limit(20).Find(&bans).Error
	return bans, err
}

func sweepExpiredBans(db *gorm.DB) {
	if db == nil {
		return
	}
	now := time.Now()
	result := db.Model(&Ban{}).
		Where("active = ? AND expires_at < ? AND expires_at != ?", true, now, time.Time{}).
		Updates(map[string]interface{}{"active": false, "deactivated_at": &now})
	if result.RowsAffected > 0 {
		loggerBans.Info("swept expired bans", "count", result.RowsAffected)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours()/24), int(d.Hours())%24)
}

// parseBanDuration parses a duration string with support for days (e.g. "7d",
// "30m", "1h", "2h30m"). Days are converted to hours before passing to
// time.ParseDuration since it doesn't support the "d" suffix.
func parseBanDuration(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		numStr := s[:len(s)-1]
		days, err := strconv.Atoi(numStr)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration: %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
