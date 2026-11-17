package transport

import (
	"cadenceforge/internal/domain"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// delivery exposes the confirmed segment list that both review and packaging
// consumers share. Only manifest records are reported.
func (a API) delivery(w http.ResponseWriter, r *http.Request) {
	view, err := a.Service.Delivery(r.PathValue("id"))
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, view)
}

// segment serves one confirmed segment. Unconfirmed or cancelled segments are
// refused even if their files happen to exist.
func (a API) segment(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		failure(w, domain.Invalid("segment index must be a non-negative integer"))
		return
	}
	_, path, err := a.Service.SegmentPath(r.PathValue("id"), r.PathValue("rendition"), index)
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

// packageDelivery emits the complete multi-rendition package. It fails unless
// the manifest is complete and every confirmed file re-verifies.
func (a API) packageDelivery(w http.ResponseWriter, r *http.Request) {
	f, err := os.CreateTemp("", "cadence-package-*.zip")
	if err != nil {
		failure(w, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = a.Service.DeliveryBundle(r.Context(), r.PathValue("id"), f); err != nil {
		failure(w, err)
		return
	}
	if _, err = f.Seek(0, 0); err != nil {
		failure(w, err)
		return
	}
	info, err := f.Stat()
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="delivery.zip"`)
	http.ServeContent(w, r, "delivery.zip", info.ModTime(), f)
}
