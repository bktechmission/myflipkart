// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Photo evidence for returns. A teammate can ask for a photo of the item; the
// customer uploads it into the same chat. Uploads are size-capped, must decode
// as a real JPEG or PNG within sane dimensions (no decompression bombs), and are
// re-encoded as JPEG, which drops EXIF metadata such as GPS location.
// Only the customer who uploaded a photo and admins can view it.

package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"image"
	"image/jpeg"
	_ "image/png" // register the PNG decoder
	"io"
	"net/http"
	"time"

	"asksauby/internal/auth"
	"asksauby/internal/model"
)

const (
	maxPhotoBytes  = 4 << 20
	maxPhotoPixels = 12_000_000 // ~4000x3000; decoding allocates width*height*8 bytes at worst
)

// photoSlots bounds concurrent image decodes so uploads can't exhaust memory.
var photoSlots = make(chan struct{}, 2)

func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	sid := r.PathValue("sid")
	if !s.ownsSID(sess.CID, sid) || len(sess.CID) == 0 || sess.CID[0] == 'G' {
		writeJSON(w, 400, map[string]string{"error": "sign in to share a photo"})
		return
	}
	t, owned := s.Hub.HumanOwns(sess.CID, sid)
	if !owned {
		writeJSON(w, 409, map[string]string{"error": "photos can be shared while a teammate is in the chat"})
		return
	}
	if ok, _ := s.chatLimit.Allow("photo:" + sess.CID); !ok {
		writeJSON(w, 429, map[string]string{"error": "too many photos at once, please wait a moment"})
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "photo is too large (4 MB max)"})
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "jpeg" && format != "png") || cfg.Width < 16 || cfg.Height < 16 || cfg.Width > 8000 || cfg.Height > 8000 || cfg.Width*cfg.Height > maxPhotoPixels {
		writeJSON(w, 415, map[string]string{"error": "please share a JPEG or PNG photo"})
		return
	}
	select {
	case photoSlots <- struct{}{}:
		defer func() { <-photoSlots }()
	case <-time.After(5 * time.Second):
		writeJSON(w, 503, map[string]string{"error": "busy, please try again"})
		return
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		writeJSON(w, 415, map[string]string{"error": "that file isn't a readable photo"})
		return
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 82}); err != nil {
		writeJSON(w, 500, map[string]string{"error": "couldn't save the photo"})
		return
	}
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if err := s.store.SavePhoto(sess.CID, id, out.Bytes()); err != nil {
		writeJSON(w, 500, map[string]string{"error": "couldn't save the photo"})
		return
	}
	s.store.AttachEvidence(sess.CID, id)
	s.store.AppendMessage(sess.CID, sid, model.Message{TS: time.Now().UTC(), Role: "user", Content: "Shared a photo",
		Meta: map[string]any{"photo": "/api/photos/" + sess.CID + "/" + id}})
	s.Hub.CustomerPhoto(t)
	writeJSON(w, 200, map[string]any{"ok": true, "url": "/api/photos/" + sess.CID + "/" + id})
}

func (s *Server) photo(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	cid, id := r.PathValue("cid"), r.PathValue("id")
	if cid != sess.CID && sess.Role != "admin" { // same answer either way: don't reveal it exists
		http.NotFound(w, r)
		return
	}
	b, err := s.store.Photo(cid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}
