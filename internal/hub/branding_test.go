package hub

import (
	"bytes"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func TestBrandingAvailableBeforeLogin(t *testing.T) {
	ui := testHub(t, "local").UIHandler()
	page := request(ui, "GET", "/login", "", nil)
	for _, want := range []string{"<title>keiryō", `rel="icon" type="image/svg+xml" href="/static/keiryo.svg"`, `src="/static/keiryo.svg"`, ">keiryō</a>"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("login missing branding: %s", want)
		}
	}
	for _, asset := range []struct {
		name, contentType string
		size              int
	}{
		{"keiryo.svg", "image/svg+xml", 0},
		{"keiryo-32.png", "image/png", 32},
		{"keiryo-512.png", "image/png", 512},
	} {
		t.Run(asset.name, func(t *testing.T) {
			w := request(ui, "GET", "/static/"+asset.name, "", nil)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != asset.contentType || len(w.Result().Cookies()) != 0 {
				t.Fatalf("asset not publicly served: status %d, type %s, cookies %v", w.Code, w.Header().Get("Content-Type"), w.Result().Cookies())
			}
			if asset.size > 0 {
				img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				if img.Bounds().Dx() != asset.size || img.Bounds().Dy() != asset.size {
					t.Fatalf("incorrect export dimensions: %v", img.Bounds())
				}
				if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
					t.Fatal("PNG background must be transparent")
				}
			}
		})
	}
}
