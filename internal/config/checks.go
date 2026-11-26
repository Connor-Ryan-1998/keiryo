package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
)

// HTTPCheck is a hub-side HTTP(S) probe. Zero TLS days disable that certificate
// threshold; an empty ExpectStatus accepts any 2xx or 3xx response.
type HTTPCheck struct {
	Name            string `json:"name"`
	URL             string `json:"url"`
	IntervalSeconds int    `json:"interval_seconds"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	ExpectStatus    []int  `json:"expect_status,omitempty"`
	FollowRedirects bool   `json:"follow_redirects"`
	TLSWarnDays     int    `json:"tls_warn_days"`
	TLSCriticalDays int    `json:"tls_critical_days"`
	Severity        string `json:"severity"`
}

// UnmarshalJSON applies defaults before decoding, so explicit zeros are kept,
// and rejects unknown fields like the rest of the configuration.
func (c *HTTPCheck) UnmarshalJSON(b []byte) error {
	type plain HTTPCheck
	v := plain{IntervalSeconds: 60, TimeoutSeconds: 10, TLSWarnDays: 14, TLSCriticalDays: 3, Severity: "critical"}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*c = HTTPCheck(v)
	return nil
}

func (c Hub) ValidateHTTPChecks() error {
	if len(c.HTTPChecks) > 256 {
		return errors.New("too many http_checks")
	}
	seen := map[string]bool{}
	for _, check := range c.HTTPChecks {
		u, err := url.Parse(check.URL)
		valid := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Fragment == ""
		valid = valid && ValidID(check.Name) && !seen[check.Name]
		valid = valid && check.IntervalSeconds >= 10 && check.IntervalSeconds <= 3600 && check.TimeoutSeconds >= 1 && check.TimeoutSeconds <= 60 && check.TimeoutSeconds < check.IntervalSeconds
		valid = valid && len(check.ExpectStatus) <= 16 && (check.Severity == "warning" || check.Severity == "critical")
		valid = valid && check.TLSWarnDays >= 0 && check.TLSWarnDays <= 365 && check.TLSCriticalDays >= 0 && (check.TLSWarnDays == 0 || check.TLSCriticalDays <= check.TLSWarnDays)
		for _, status := range check.ExpectStatus {
			valid = valid && status >= 100 && status <= 599
		}
		if !valid {
			return errors.New("http_checks require unique names, HTTP(S) URLs without credentials or fragment, interval 10-3600s, timeout 1-60s below the interval, status codes 100-599, TLS days 0-365 with critical <= warning, and warning/critical severity")
		}
		seen[check.Name] = true
	}
	return nil
}
