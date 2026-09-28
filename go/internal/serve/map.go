package serve

import (
	"embed"
	"io/fs"
	"net/http"
	"time"

	"github.com/ptudor/navlistener/internal/orbitref"
)

//go:embed map/*
var mapAssets embed.FS

// SetMapReference is called before the server starts. The reference contains
// public geometry only; observation evidence always comes from the request view.
func (s *Server) SetMapReference(c *orbitref.Catalogue) { s.mapReference = c }

func (s *Server) mapReferenceSnapshot(now time.Time) orbitref.Snapshot {
	if s.mapReference == nil {
		return orbitref.Snapshot{Source: orbitref.Source, Status: "unavailable", Satellites: []orbitref.Satellite{}}
	}
	return s.mapReference.Snapshot(now)
}

func mapHandler() http.Handler {
	assets, err := fs.Sub(mapAssets, "map")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/gnss/map/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if methodNotAllowedGetHead(w, r) {
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
