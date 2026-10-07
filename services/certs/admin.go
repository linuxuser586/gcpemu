package certs

import (
	"net/http"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// serveRotate implements POST /_emu/v1/ca/rotate (`gcpemu ca rotate` on a
// running instance): it replaces the instance CA and re-issues every
// managed certificate from the new root before returning. Self-managed
// certificates are the user's and are left alone.
func (s *Service) serveRotate(w http.ResponseWriter, r *http.Request) {
	if s.env.CA == nil {
		apierr.Write(w, apierr.FailedPrecondition("The instance has no CA."))
		return
	}
	if err := s.env.CA.Rotate(s.env.Config.Instance); err != nil {
		apierr.Write(w, apierr.Internal("rotate CA: %v", err))
		return
	}
	s.invalidate()
	s.reconcileAll(r.Context())
	writeJSON(w, map[string]string{"status": "rotated", "path": s.env.CA.Path()})
}
