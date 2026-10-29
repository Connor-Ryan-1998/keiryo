package hub

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/otlp"
)

func writeOTLP(w http.ResponseWriter, r *http.Request, status int, message proto.Message) {
	content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var body []byte
	var err error
	if content == "application/json" {
		body, err = protojson.Marshal(message)
	} else {
		content = "application/x-protobuf"
		body, err = proto.Marshal(message)
	}
	if err != nil {
		status = 500
		body = nil
	}
	w.Header().Set("Content-Type", content)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(body)
}
func writeOTLPError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeOTLP(w, r, status, &statuspb.Status{Message: message})
}
func (h *Hub) ingestOTLP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOTLPError(w, r, 405, "POST required")
		return
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (content != "application/x-protobuf" && content != "application/json") {
		writeOTLPError(w, r, 415, "use application/x-protobuf or application/json")
		return
	}
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			var large *http.MaxBytesError
			if errors.As(err, &large) {
				writeOTLPError(w, r, 413, "request too large")
			} else {
				writeOTLPError(w, r, 400, "invalid gzip request")
			}
			return
		}
		defer gz.Close()
		reader = gz
	default:
		writeOTLPError(w, r, 415, "unsupported content encoding")
		return
	}
	body, err := io.ReadAll(io.LimitReader(reader, h.config.MaxBodyBytes+1))
	var large *http.MaxBytesError
	if len(body) > int(h.config.MaxBodyBytes) || errors.As(err, &large) {
		writeOTLPError(w, r, 413, "request too large")
		return
	}
	if err != nil {
		writeOTLPError(w, r, 400, "invalid request body")
		return
	}
	req, err := otlp.Decode(body, content)
	if err != nil {
		writeOTLPError(w, r, 400, "invalid OTLP metrics")
		return
	}
	now := time.Now()
	points, rejected, err := otlp.Normalize(req, r.Header.Get("X-Host-ID"), now, time.Duration(h.config.RawHours)*time.Hour, time.Duration(h.config.FutureSeconds)*time.Second, h.config.MaxEntities)
	if err != nil {
		status := 400
		if errors.Is(err, otlp.ErrHostIdentity) {
			status = 403
		}
		writeOTLPError(w, r, status, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := h.store.IngestOTLP(ctx, r.Header.Get("X-Host-ID"), points, now, h.config.BucketSeconds)
	if err != nil {
		h.setError("OTLP metrics could not be committed; check database and disk capacity.")
		writeOTLPError(w, r, 503, "storage unavailable")
		return
	}
	out := new(collectorpb.ExportMetricsServiceResponse)
	rejected += result.Rejected
	if rejected > 0 {
		out.PartialSuccess = &collectorpb.ExportMetricsPartialSuccess{RejectedDataPoints: rejected, ErrorMessage: "Unsupported, invalid or conflicting metric points rejected; see the supported metric mapping."}
	}
	if result.Accepted > 0 {
		h.notify(r.Header.Get("X-Host-ID"))
	}
	writeOTLP(w, r, 200, out)
}
