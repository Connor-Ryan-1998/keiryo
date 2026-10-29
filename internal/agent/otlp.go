package agent

import (
	"bytes"
	"context"
	"errors"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type retryAfterError struct{ after time.Duration }

func (e *retryAfterError) Error() string { return "OTLP temporarily unavailable" }

func (a *Agent) exportMetrics(ctx context.Context, s protocol.Sample) error {
	request, err := otlp.EncodeSample(s)
	if err != nil {
		return errRejected
	}
	body, err := proto.Marshal(request)
	if err != nil || len(body) > 2<<20 {
		return errRejected
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.Config.MetricsURL(), bytes.NewReader(body))
	if err != nil {
		return errRejected
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("X-Host-ID", a.Config.HostID)
	res, err := a.client.Do(req)
	if err != nil {
		return errors.New("OTLP endpoint unavailable")
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 429, 502, 503, 504:
		delay := time.Duration(0)
		raw := res.Header.Get("Retry-After")
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 300)) * time.Second
		} else if at, err := http.ParseTime(raw); err == nil {
			delay = min(max(time.Until(at), 0), 5*time.Minute)
		}
		return &retryAfterError{delay}
	case 200:
	default:
		return errRejected
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return errRejected
	}
	response := new(collectorpb.ExportMetricsServiceResponse)
	content, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return errRejected
	}
	switch content {
	case "application/x-protobuf":
		err = proto.Unmarshal(data, response)
	case "application/json":
		err = (protojson.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 32}).Unmarshal(data, response)
	default:
		return errRejected
	}
	if err != nil {
		return errRejected
	}
	if p := response.GetPartialSuccess(); p != nil {
		slog.Warn("OTLP export partially rejected; not retrying", "rejected_points", p.RejectedDataPoints)
	}
	return nil
}
