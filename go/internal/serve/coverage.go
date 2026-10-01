package serve

import (
	"time"

	"github.com/ptudor/navlistener/internal/orbitref"
)

// SetMapReference is called before the server starts. The reference contains
// public geometry only; observation evidence always comes from the request view.
func (s *Server) SetMapReference(c *orbitref.Catalogue) { s.mapReference = c }

func (s *Server) mapReferenceSnapshot(now time.Time) orbitref.Snapshot {
	if s.mapReference == nil {
		return orbitref.Snapshot{Source: orbitref.Source, Status: "unavailable", Satellites: []orbitref.Satellite{}}
	}
	return s.mapReference.Snapshot(now)
}
