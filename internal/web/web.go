// Package web provides the HTTP API and serves the embedded user interface.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/manager"
)

//go:embed static
var static embed.FS

const (
	maxBodySize = 1 << 20
	auditLimit  = 200
)

// Auth holds the login of the manager; an empty User means read-only access.
type Auth struct {
	User     string
	Password string
	// MetricsUser is the login of /metrics, which it opens and nothing else. The login
	// of the interface does not work there. Without it, /metrics is open.
	MetricsUser     string
	MetricsPassword string
	// MetricsOff removes /metrics altogether.
	MetricsOff bool
}

type server struct {
	mgr  *manager.Manager
	auth Auth
}

type contextKey struct{}

// New builds the handler of the manager. basePath is the subpath a reverse proxy
// serves it under, if the proxy does not strip that path itself.
func New(mgr *manager.Manager, auth Auth, basePath string) http.Handler {
	s := &server{mgr: mgr, auth: auth}
	assets, _ := fs.Sub(static, "static")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/meta", s.meta)
	mux.HandleFunc("GET /api/dashboard", s.dashboard)
	mux.HandleFunc("GET /api/leases", s.leases)
	mux.HandleFunc("GET /api/reservations", s.reservations)
	mux.HandleFunc("GET /api/subnets", s.subnets)
	mux.HandleFunc("GET /api/devices", s.devices)
	mux.HandleFunc("DELETE /api/leases/{ip}", s.writer(s.deleteLease))
	mux.HandleFunc("POST /api/reservations", s.writer(s.createReservation))
	mux.HandleFunc("POST /api/reservations/check", s.writer(s.checkReservation))
	mux.HandleFunc("POST /api/reservations/{subnet}/{type}/{id}/check", s.writer(s.checkReservationUpdate))
	mux.HandleFunc("PUT /api/reservations/{subnet}/{type}/{id}", s.writer(s.updateReservation))
	mux.HandleFunc("DELETE /api/reservations/{subnet}/{type}/{id}", s.writer(s.deleteReservation))
	mux.HandleFunc("POST /api/subnets/{subnet}/check", s.writer(s.checkSubnet))
	mux.HandleFunc("PUT /api/subnets/{subnet}", s.writer(s.updateSubnet))
	mux.HandleFunc("GET /api/classes", s.classes)
	mux.HandleFunc("POST /api/classes", s.writer(s.createClass))
	mux.HandleFunc("PUT /api/classes/{name}", s.writer(s.updateClass))
	mux.HandleFunc("DELETE /api/classes/{name}", s.writer(s.deleteClass))
	mux.HandleFunc("POST /api/config/write", s.writer(s.persist))
	mux.HandleFunc("GET /api/servers", s.servers)
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/settings", s.settings)
	mux.HandleFunc("PUT /api/settings", s.writer(s.updateSettings))
	mux.HandleFunc("POST /api/findings/hide", s.writer(s.hideFinding))
	mux.HandleFunc("GET /api/clients/{id}", s.client)
	mux.HandleFunc("POST /api/servers/{name}/ha/{action}", s.writer(s.haAction))
	mux.HandleFunc("POST /api/servers/{name}/sync", s.writer(s.syncServer))
	mux.HandleFunc("GET /api/audit", s.writer(s.audit))
	mux.HandleFunc("GET /api/backups", s.writer(s.backups))
	mux.HandleFunc("GET /api/backups/{name}", s.writer(s.backup))
	mux.Handle("GET /", revalidate(http.FileServerFS(assets)))

	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-origin request rejected; behind a reverse proxy, pass the original Host or X-Forwarded-Host header"})
	}))
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	root.Handle("/", protection.Handler(s.authenticate(noStore(mux))))
	if !auth.MetricsOff {
		root.HandleFunc("GET /metrics", s.metricsLogin)
	}
	return forwardedHost(mount(basePath, secureHeaders(root)))
}

// noStore keeps API answers out of caches: they hold client data, and backups hold
// the credentials of the Kea configuration.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// revalidate keeps a browser from running an old frontend after an update.
func revalidate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// forwardedHost restores the host the browser used. A reverse proxy that rewrites
// Host would otherwise make every write look cross-origin. A foreign page cannot
// use this to get around the check: setting the header cross-origin needs a CORS
// preflight, which the manager never allows.
func forwardedHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); host != "" {
			r.Host = strings.TrimSpace(host)
		}
		next.ServeHTTP(w, r)
	})
}

// mount serves next below basePath and keeps /healthz at the root for the
// container health check.
func mount(basePath string, next http.Handler) http.Handler {
	if basePath == "" {
		return next
	}
	mux := http.NewServeMux()
	mux.Handle("/healthz", next)
	mux.Handle(basePath+"/", http.StripPrefix(basePath, next))
	return mux
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *server) metricsLogin(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if s.auth.MetricsUser != "" && (!ok || !equal(user, s.auth.MetricsUser) || !equal(password, s.auth.MetricsPassword)) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kea Roost metrics", charset="UTF-8"`)
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "login required"})
		return
	}
	s.metrics(w, r)
}

func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth.User == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || !equal(user, s.auth.User) || !equal(password, s.auth.Password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Kea Roost", charset="UTF-8"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "login required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, user)))
	})
}

// writer guards everything that changes Kea or exposes configuration contents.
func (s *server) writer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.User == "" {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "read-only mode: MANAGER_USER and MANAGER_PASSWORD are not set"})
			return
		}
		next(w, r)
	}
}

func user(r *http.Request) string {
	u, _ := r.Context().Value(contextKey{}).(string)
	return u
}

type errorBody struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var validation *manager.ValidationError
	switch {
	case errors.As(err, &validation):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: validation.Problems})
	case errors.Is(err, manager.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "entry not found"})
	case errors.Is(err, manager.ErrARPMonitorDisabled):
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error()})
	default:
		var ce *kea.CommandError
		if !errors.As(err, &ce) {
			slog.Warn("kea request", "error", err)
		}
		writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
	}
}

func writeResult(w http.ResponseWriter, res *manager.Result) {
	status := http.StatusOK
	if res.Status != manager.StatusOK {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, res)
}

func (s *server) meta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"serverName": s.mgr.ServerName(),
		"writable":   s.auth.User != "",
		"devices":    s.mgr.DevicesEnabled(),
		"peers":      s.mgr.HasPeers(),
		"version":    s.mgr.Version(),
		"user":       user(r),
	})
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Dashboard(r.Context()))
}

func (s *server) leases(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.LeasesWithReservations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) reservations(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Reservations(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) subnets(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Subnets(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) devices(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Devices(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) deleteLease(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteLease(r.Context(), user(r), r.PathValue("ip")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readInput(w http.ResponseWriter, r *http.Request) (*manager.ReservationInput, bool) {
	var in manager.ReservationInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "cannot read request"})
		return nil, false
	}
	return &in, true
}

func (s *server) writeCheck(w http.ResponseWriter, r *http.Request, in *manager.ReservationInput, update bool) {
	warnings, hints, err := s.mgr.CheckReservation(r.Context(), *in, update)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": warnings, "hints": hints})
}

func (s *server) checkReservation(w http.ResponseWriter, r *http.Request) {
	if in, ok := readInput(w, r); ok {
		s.writeCheck(w, r, in, false)
	}
}

func (s *server) checkReservationUpdate(w http.ResponseWriter, r *http.Request) {
	in, ok := readInput(w, r)
	if !ok {
		return
	}
	subnet, ok := subnetParam(w, r)
	if !ok {
		return
	}
	in.SubnetID, in.IdentifierType, in.Identifier = subnet, r.PathValue("type"), r.PathValue("id")
	s.writeCheck(w, r, in, true)
}

func (s *server) createReservation(w http.ResponseWriter, r *http.Request) {
	in, ok := readInput(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.CreateReservation(r.Context(), user(r), *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func subnetParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("subnet"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid subnet ID"})
		return 0, false
	}
	return id, true
}

func (s *server) updateReservation(w http.ResponseWriter, r *http.Request) {
	in, ok := readInput(w, r)
	if !ok {
		return
	}
	subnet, ok := subnetParam(w, r)
	if !ok {
		return
	}
	in.SubnetID, in.IdentifierType, in.Identifier = subnet, r.PathValue("type"), r.PathValue("id")
	res, err := s.mgr.UpdateReservation(r.Context(), user(r), *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) deleteReservation(w http.ResponseWriter, r *http.Request) {
	subnet, ok := subnetParam(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.DeleteReservation(r.Context(), user(r), subnet, r.PathValue("type"), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func readSubnet(w http.ResponseWriter, r *http.Request) (int64, *manager.SubnetInput, bool) {
	id, ok := subnetParam(w, r)
	if !ok {
		return 0, nil, false
	}
	var in manager.SubnetInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "cannot read request"})
		return 0, nil, false
	}
	return id, &in, true
}

func (s *server) checkSubnet(w http.ResponseWriter, r *http.Request) {
	id, in, ok := readSubnet(w, r)
	if !ok {
		return
	}
	warnings, err := s.mgr.CheckSubnet(r.Context(), id, *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": warnings})
}

func (s *server) updateSubnet(w http.ResponseWriter, r *http.Request) {
	id, in, ok := readSubnet(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.UpdateSubnet(r.Context(), user(r), id, *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) classes(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.Classes(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func readClass(w http.ResponseWriter, r *http.Request) (*manager.ClassInput, bool) {
	var in manager.ClassInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "cannot read request"})
		return nil, false
	}
	return &in, true
}

func (s *server) createClass(w http.ResponseWriter, r *http.Request) {
	in, ok := readClass(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.CreateClass(r.Context(), user(r), *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) updateClass(w http.ResponseWriter, r *http.Request) {
	in, ok := readClass(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.UpdateClass(r.Context(), user(r), r.PathValue("name"), *in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) deleteClass(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.DeleteClass(r.Context(), user(r), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) persist(w http.ResponseWriter, r *http.Request) {
	writeResult(w, s.mgr.Persist(r.Context(), user(r)))
}

func (s *server) servers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Servers(r.Context()))
}

func (s *server) haAction(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.RunHA(r.Context(), user(r), r.PathValue("name"), r.PathValue("action"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) settings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.SettingFields())
}

func (s *server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var values map[string]int
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(&values); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON"})
		return
	}
	if err := s.mgr.UpdateSettings(user(r), values); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.SettingFields())
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := w.Write(s.mgr.Metrics(r.Context())); err != nil {
		slog.Debug("write metrics", "error", err)
	}
}

func (s *server) hideFinding(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rule   string `json:"rule"`
		Target string `json:"target"`
		Hidden bool   `json:"hidden"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON"})
		return
	}
	if err := s.mgr.HideFinding(user(r), in.Rule, in.Target, in.Hidden); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"hidden": in.Hidden})
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Health(r.Context()))
}

func (s *server) client(w http.ResponseWriter, r *http.Request) {
	client, err := s.mgr.Client(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, client)
}

func (s *server) syncServer(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.SyncServer(r.Context(), user(r), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeResult(w, res)
}

func (s *server) audit(w http.ResponseWriter, _ *http.Request) {
	entries, err := s.mgr.Store().AuditTail(auditLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *server) backups(w http.ResponseWriter, _ *http.Request) {
	list, err := s.mgr.Store().Backups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) backup(w http.ResponseWriter, r *http.Request) {
	path, err := s.mgr.Store().BackupPath(r.PathValue("name"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}
