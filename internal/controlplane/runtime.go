package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Run starts control-plane modules according to the configured role.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	store, err := NewS3StateStore(ctx, cfg.State.S3)
	if err != nil {
		return err
	}

	var servers []*http.Server
	errCh := make(chan error, 4)

	if roleEnabled(cfg.Role, RoleRegistry) {
		reg, err := NewRegistryServer(cfg, store, logger)
		if err != nil {
			return err
		}
		srv, err := reg.HTTPServer()
		if err != nil {
			return err
		}
		servers = append(servers, srv)
		go serveTLS(srv, logger, "registry", errCh)
	}

	if roleEnabled(cfg.Role, RoleEvents) {
		ev := NewEventsServer(cfg, store, logger)
		srv, err := ev.HTTPServer()
		if err != nil {
			return err
		}
		servers = append(servers, srv)
		go serveTLS(srv, logger, "events", errCh)
	}

	if roleEnabled(cfg.Role, RoleController) {
		controller := NewController(cfg, store, logger)
		go func() {
			logger.Info("starting controller", "id", controller.id)
			if err := controller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("controller failed: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			shutdownServers(servers, logger)
			return err
		}
	}

	shutdownServers(servers, logger)
	return ctx.Err()
}

func roleEnabled(role, want string) bool {
	return role == RoleAll || role == want
}

func serveTLS(srv *http.Server, logger *slog.Logger, name string, errCh chan<- error) {
	logger.Info("starting server", "role", name, "addr", srv.Addr)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s server failed: %w", name, err)
	}
}

func shutdownServers(servers []*http.Server, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		logger.Info("stopping server", "addr", srv.Addr)
		_ = srv.Shutdown(ctx)
	}
}
