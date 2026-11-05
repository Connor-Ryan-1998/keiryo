package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

// Notification tracks the latest desired state and the last acknowledged delivery.
type Notification struct {
	State, Delivered, Kind, Detail string
	Since, Next                    int64
	Attempts                       int
}

func (s *Store) Notifications(ctx context.Context, host string) (map[string]Notification, error) {
	value, err := s.Setting(ctx, "notifications/"+host)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]Notification{}, nil
	}
	if err != nil {
		return nil, err
	}
	states := map[string]Notification{}
	if err = json.Unmarshal([]byte(value), &states); err != nil {
		return nil, err
	}
	return states, nil
}

func (s *Store) SaveNotifications(ctx context.Context, host string, states map[string]Notification) error {
	if len(states) > 4096 {
		return errors.New("notification state limit exceeded")
	}
	data, err := json.Marshal(states)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, "notifications/"+host, string(data))
}

func (s *Store) NotificationRetryAt(ctx context.Context) (int64, error) {
	value, err := s.Setting(ctx, "discord_retry_at")
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

func (s *Store) SetNotificationRetryAt(ctx context.Context, at int64) error {
	return s.SetSetting(ctx, "discord_retry_at", strconv.FormatInt(at, 10))
}
