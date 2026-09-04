package serve

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/ptudor/navlistener/internal/identity"
)

type connectionContextKey struct{}

// A policy reset closes transports with in-flight responses for that audience.
// Closing a socket interrupts blocked writes without making ingest wait for a
// slow consumer. Bytes already accepted by the transport before Close cannot
// be retracted. An incomplete response is retried in the new epoch by clients.
type responseDelivery struct {
	server     *Server
	audience   string
	generation uint64
	connection net.Conn
	obsolete   atomic.Bool
}

func connectionContext(ctx context.Context, connection net.Conn) context.Context {
	return context.WithValue(ctx, connectionContextKey{}, connection)
}

func (s *Server) beginDelivery(r *http.Request, selected identity.Audience) *responseDelivery {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	generation, _ := s.policyEpochs.Current(selected.Key())
	connection, _ := r.Context().Value(connectionContextKey{}).(net.Conn)
	d := &responseDelivery{server: s, audience: selected.Key(), generation: generation, connection: connection}
	if s.deliveries == nil {
		s.deliveries = make(map[*responseDelivery]struct{})
	}
	s.deliveries[d] = struct{}{}
	return d
}

func (d *responseDelivery) current() bool {
	generation, _ := d.server.policyEpochs.Current(d.audience)
	return !d.obsolete.Load() && generation == d.generation
}

func (d *responseDelivery) finish() {
	d.server.deliveryMu.Lock()
	delete(d.server.deliveries, d)
	d.server.deliveryMu.Unlock()
}

func (s *Server) invalidateDeliveries(selected identity.Audience) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	for d := range s.deliveries {
		if d.audience != selected.Key() {
			continue
		}
		d.obsolete.Store(true)
		if d.connection != nil {
			_ = d.connection.Close()
		}
	}
}

func (d *responseDelivery) write(w http.ResponseWriter, body []byte) {
	if !d.current() {
		writeError(w, http.StatusServiceUnavailable, "audience policy changed; retry request")
		return
	}
	if _, err := w.Write(body); err == nil {
		// Flush while the transport is still registered for revocation. net/http
		// otherwise flushes small buffered bodies after the handler has returned.
		_ = http.NewResponseController(w).Flush()
	}
}
