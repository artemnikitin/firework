package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed web/*
var visibilityWeb embed.FS

type VisibilityServer struct {
	cfg        Config
	service    *VisibilityService
	logger     *slog.Logger
	sessionKey [sha256.Size]byte
}

func NewVisibilityServer(cfg Config, store StateStore, logger *slog.Logger) *VisibilityServer {
	return &VisibilityServer{cfg: cfg, service: NewVisibilityService(cfg, store), logger: logger, sessionKey: sha256.Sum256([]byte("firework-operator-session:" + cfg.OperatorToken))}
}

func (s *VisibilityServer) HTTPServer() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
	})
	mux.HandleFunc("GET /v1/nodes", s.auth(s.handleNodes))
	mux.HandleFunc("GET /v1/nodes/{id}", s.auth(s.handleNode))
	mux.HandleFunc("GET /v1/services", s.auth(s.handleServices))
	mux.HandleFunc("GET /v1/services/{name}", s.auth(s.handleService))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	assets, err := fs.Sub(visibilityWeb, "web")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /", s.handleIndex)

	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading api tls keypair: %w", err)
	}
	return &http.Server{
		Addr: s.cfg.APIListenAddr, Handler: securityHeaders(mux),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}, nil
}

func (s *VisibilityServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="firework-operator"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator authentication required"})
			return
		}
		next(w, r)
	}
}

func (s *VisibilityServer) authorized(r *http.Request) bool {
	want := []byte(s.cfg.OperatorToken)
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if provided != "" && subtle.ConstantTimeCompare([]byte(provided), want) == 1 {
		return true
	}
	cookie, err := r.Cookie("firework_operator_session")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(fmt.Sprintf("%x", s.sessionKey))) == 1
}

func (s *VisibilityServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.Nodes(r.Context(), r.URL.Query().Get("state"))
	respondVisibility(w, items, err)
}

func (s *VisibilityServer) handleNode(w http.ResponseWriter, r *http.Request) {
	item, found, err := s.service.Node(r.Context(), r.PathValue("id"))
	if err != nil {
		respondVisibility(w, nil, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *VisibilityServer) handleServices(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	items, err := s.service.Services(r.Context(), query.Get("state"), query.Get("health"), query.Get("node"))
	respondVisibility(w, items, err)
}

func (s *VisibilityServer) handleService(w http.ResponseWriter, r *http.Request) {
	item, found, err := s.service.Service(r.Context(), r.PathValue("name"))
	if err != nil {
		respondVisibility(w, nil, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "service not found"})
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func respondVisibility(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read deployment state"})
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *VisibilityServer) handleLoginPage(w http.ResponseWriter, _ *http.Request) {
	s.serveWebFile(w, "login.html")
}

func (s *VisibilityServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(s.cfg.OperatorToken)) != 1 {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "firework_operator_session", Value: fmt.Sprintf("%x", s.sessionKey), Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *VisibilityServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "firework_operator_session", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *VisibilityServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r) {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	s.serveWebFile(w, "index.html")
}

func (s *VisibilityServer) serveWebFile(w http.ResponseWriter, name string) {
	data, err := visibilityWeb.ReadFile("web/" + name)
	if err != nil {
		http.Error(w, "embedded UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
