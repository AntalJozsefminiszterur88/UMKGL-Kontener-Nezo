package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

const (
	defaultBindAddr        = ":8080"
	defaultActionTimeout   = 30 * time.Second
	defaultStopTimeoutSecs = 120
	defaultHealthTimeout   = 5 * time.Second
)

type config struct {
	BindAddr        string
	AuthToken       string
	TargetContainer string
	ActionTimeout   time.Duration
	StopTimeout     int
}

type apiServer struct {
	docker *client.Client
	cfg    config
	mu     sync.Mutex
}

type errorResponse struct {
	Error string `json:"error"`
}

type healthResponse struct {
	Status     string `json:"status"`
	Target     string `json:"target"`
	APIVersion string `json:"api_version,omitempty"`
	Error      string `json:"error,omitempty"`
}

type factorioStateResponse struct {
	Target         string `json:"target"`
	Running        bool   `json:"running"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Health         string `json:"health,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

type factorioActionResponse struct {
	Action    string                `json:"action"`
	Changed   bool                  `json:"changed"`
	Message   string                `json:"message"`
	Container factorioStateResponse `json:"container"`
}

func main() {
	cfg := loadConfig()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}
	defer dockerClient.Close()

	server := &apiServer{
		docker: dockerClient,
		cfg:    cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/api/v1/factorio/status", server.withAuth(server.handleStatus))
	mux.HandleFunc("/api/v1/factorio/start", server.withAuth(server.requireMethod(http.MethodPost, server.handleStart)))
	mux.HandleFunc("/api/v1/factorio/stop", server.withAuth(server.requireMethod(http.MethodPost, server.handleStop)))

	httpServer := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           server.logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	go func() {
		<-shutdownCtx.Done()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("control API shutdown error: %v", err)
		}
	}()

	log.Printf(
		"Factorio control API listening on %s for container %s",
		cfg.BindAddr,
		cfg.TargetContainer,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("control API stopped unexpectedly: %v", err)
	}
}

func loadConfig() config {
	authToken := strings.TrimSpace(os.Getenv("CONTROL_API_TOKEN"))
	if authToken == "" {
		log.Fatal("CONTROL_API_TOKEN must be set")
	}

	targetContainer := strings.TrimSpace(os.Getenv("CONTROL_TARGET_CONTAINER"))
	if targetContainer == "" {
		targetContainer = "factorio-server"
	}

	bindAddr := strings.TrimSpace(os.Getenv("CONTROL_API_BIND_ADDR"))
	if bindAddr == "" {
		bindAddr = defaultBindAddr
	}

	stopTimeout := defaultStopTimeoutSecs
	if rawTimeout := strings.TrimSpace(os.Getenv("CONTROL_STOP_TIMEOUT_SECONDS")); rawTimeout != "" {
		parsedTimeout, err := strconv.Atoi(rawTimeout)
		if err != nil || parsedTimeout < 0 {
			log.Fatalf("invalid CONTROL_STOP_TIMEOUT_SECONDS=%q", rawTimeout)
		}
		stopTimeout = parsedTimeout
	}

	return config{
		BindAddr:        bindAddr,
		AuthToken:       authToken,
		TargetContainer: targetContainer,
		ActionTimeout:   defaultActionTimeout,
		StopTimeout:     stopTimeout,
	}
}

func (s *apiServer) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing bearer token"})
			return
		}

		token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if token != s.cfg.AuthToken {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid bearer token"})
			return
		}

		next(w, r)
	}
}

func (s *apiServer) requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}

		next(w, r)
	}
}

func (s *apiServer) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func (s *apiServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultHealthTimeout)
	defer cancel()

	ping, err := s.docker.Ping(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{
			Status: "unhealthy",
			Target: s.cfg.TargetContainer,
			Error:  err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, healthResponse{
		Status:     "ok",
		Target:     s.cfg.TargetContainer,
		APIVersion: ping.APIVersion,
	})
}

func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}

	state, err := s.inspectFactorioState(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, state)
}

func (s *apiServer) handleStart(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, inspectResponse, err := s.inspectFactorioStateWithRaw(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}

	if inspectResponse.State != nil && inspectResponse.State.Running {
		writeJSON(w, http.StatusOK, factorioActionResponse{
			Action:    "start",
			Changed:   false,
			Message:   "Factorio server is already running",
			Container: state,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	err = s.docker.ContainerStart(ctx, s.cfg.TargetContainer, dockercontainer.StartOptions{})
	cancel()
	if err != nil {
		writeDockerError(w, err)
		return
	}

	updatedState, err := s.inspectFactorioState(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, factorioActionResponse{
		Action:    "start",
		Changed:   true,
		Message:   "Factorio server started",
		Container: updatedState,
	})
}

func (s *apiServer) handleStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, inspectResponse, err := s.inspectFactorioStateWithRaw(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}

	if inspectResponse.State == nil || !inspectResponse.State.Running {
		writeJSON(w, http.StatusOK, factorioActionResponse{
			Action:    "stop",
			Changed:   false,
			Message:   "Factorio server is already stopped",
			Container: state,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ActionTimeout)
	err = s.docker.ContainerStop(ctx, s.cfg.TargetContainer, s.newStopOptions())
	cancel()
	if err != nil {
		writeDockerError(w, err)
		return
	}

	updatedState, err := s.inspectFactorioState(r.Context())
	if err != nil {
		writeDockerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, factorioActionResponse{
		Action:    "stop",
		Changed:   true,
		Message:   "Factorio server stopped",
		Container: updatedState,
	})
}

func (s *apiServer) inspectFactorioState(ctx context.Context) (factorioStateResponse, error) {
	state, _, err := s.inspectFactorioStateWithRaw(ctx)
	return state, err
}

func (s *apiServer) inspectFactorioStateWithRaw(ctx context.Context) (factorioStateResponse, dockercontainer.InspectResponse, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, s.cfg.ActionTimeout)
	defer cancel()

	inspectResponse, err := s.docker.ContainerInspect(inspectCtx, s.cfg.TargetContainer)
	if err != nil {
		return factorioStateResponse{}, dockercontainer.InspectResponse{}, err
	}

	return buildFactorioStateResponse(s.cfg.TargetContainer, inspectResponse), inspectResponse, nil
}

func (s *apiServer) newStopOptions() dockercontainer.StopOptions {
	timeout := s.cfg.StopTimeout
	return dockercontainer.StopOptions{Timeout: &timeout}
}

func buildFactorioStateResponse(target string, inspectResponse dockercontainer.InspectResponse) factorioStateResponse {
	response := factorioStateResponse{
		Target:    target,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if inspectResponse.State != nil {
		response.Running = inspectResponse.State.Running
		response.Status = inspectResponse.State.Status
		response.StartedAt = inspectResponse.State.StartedAt
		response.FinishedAt = inspectResponse.State.FinishedAt
		if inspectResponse.State.Health != nil {
			response.Health = inspectResponse.State.Health.Status
		}
	}

	if response.Status == "" {
		if response.Running {
			response.Status = "running"
		} else {
			response.Status = "unknown"
		}
	}

	if inspectResponse.Config != nil && inspectResponse.Config.Labels != nil {
		response.ComposeProject = inspectResponse.Config.Labels["com.docker.compose.project"]
		response.ComposeService = inspectResponse.Config.Labels["com.docker.compose.service"]
	}

	return response
}

func writeDockerError(w http.ResponseWriter, err error) {
	if errdefs.IsNotFound(err) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("failed to encode JSON response: %v", err)
	}
}
