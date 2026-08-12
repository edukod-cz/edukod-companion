package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/config"
	"github.com/edukod-cz/edukod-companion/internal/gateway"
	"github.com/edukod-cz/edukod-companion/internal/localapi"
	"github.com/edukod-cz/edukod-companion/internal/relay"
	"github.com/edukod-cz/edukod-companion/internal/wsclient"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
}

func New() *App {
	return &App{Stdout: os.Stdout, Stderr: os.Stderr}
}

func (application *App) Run(arguments []string) int {
	if len(arguments) == 0 {
		application.usage()
		return 2
	}
	var err error
	switch arguments[0] {
	case "pair":
		err = application.pair(arguments[1:])
	case "doctor":
		err = application.doctor(arguments[1:])
	case "run":
		err = application.run(arguments[1:])
	case "status":
		err = application.status(arguments[1:])
	case "models":
		err = application.models(arguments[1:])
	case "unpair":
		err = application.unpair(arguments[1:])
	case "version", "--version", "-version":
		fmt.Fprintln(application.Stdout, Version)
		return 0
	case "help", "--help", "-h":
		application.usage()
		return 0
	default:
		fmt.Fprintf(application.Stderr, "unknown command %q\n", arguments[0])
		application.usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(application.Stderr, "edukod-companion: %v\n", err)
		return 1
	}
	return 0
}

func (application *App) usage() {
	fmt.Fprintln(application.Stderr, "Usage: edukod-companion <pair|doctor|run|status|models|unpair|version> [options]")
}

func commonConfigFlag(flags *flag.FlagSet) *string {
	return flags.String("config-dir", "", "configuration directory (default: $XDG_CONFIG_HOME/edukod-companion)")
}

func newStore(directory string) (*config.Store, error) {
	return config.NewStore(directory)
}

func (application *App) pair(arguments []string) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	school := flags.String("school", "", "EduKod school origin, for example https://school.edukod.cz")
	code := flags.String("code", "", "single-use pairing code from the EduKod admin panel")
	deviceName := flags.String("name", hostname(), "device name shown in EduKod")
	localBaseURL := flags.String("local-url", localapi.DefaultBaseURL, "loopback OpenAI-compatible /v1 URL")
	credentialStore := flags.String("credential-store", "auto", "credential storage: auto, keyring, or file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("pair does not accept positional arguments")
	}
	validatedLocalURL, err := localapi.ValidateBaseURL(*localBaseURL)
	if err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	exists, err := store.Exists()
	if err != nil {
		return err
	}
	if exists {
		return errors.New("this companion is already paired; unpair it before creating a new pairing")
	}
	switch *credentialStore {
	case "auto":
	case "keyring":
		store.RequireKeyring = true
	case "file":
		store.Keyring = nil
	default:
		return errors.New("credential-store must be auto, keyring, or file")
	}
	enrollment, err := gateway.New().Enroll(context.Background(), *school, *code, *deviceName)
	if err != nil {
		return err
	}
	origin, _ := gateway.ValidateSchoolOrigin(*school)
	state := config.State{
		SchoolOrigin: origin.String(),
		DeviceID:     enrollment.DeviceID,
		DeviceName:   strings.TrimSpace(*deviceName),
		WebSocketURL: enrollment.WebSocketURL,
		LocalBaseURL: validatedLocalURL.String(),
		PairedAt:     time.Now().UTC(),
	}
	if err := store.Save(state, enrollment.DeviceToken); err != nil {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = gateway.New().Revoke(revokeCtx, state.SchoolOrigin, enrollment.DeviceToken)
		return fmt.Errorf("save device credential: %w", err)
	}
	fmt.Fprintf(application.Stdout, "Paired %s with %s. Run `edukod-companion doctor`, then enable the user service.\n", state.DeviceName, state.SchoolOrigin)
	return nil
}

func (application *App) doctor(arguments []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	state, _, err := store.Load()
	if err != nil {
		return fmt.Errorf("pairing/config check failed: %w", err)
	}
	fmt.Fprintf(application.Stdout, "ok  paired device %s (%s)\n", state.DeviceName, state.DeviceID)
	client, err := localapi.New(state.LocalBaseURL, relay.DefaultMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("local URL check failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := client.Models(ctx)
	if err != nil {
		return fmt.Errorf("local model check failed: %w", err)
	}
	var result struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(models, &result)
	fmt.Fprintf(application.Stdout, "ok  local model API responded (%d advertised models)\n", len(result.Data))
	fmt.Fprintln(application.Stdout, "ok  configuration is ready; the persistent WSS connection is tested by `run`")
	return nil
}

func (application *App) models(arguments []string) error {
	flags := flag.NewFlagSet("models", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	state, _, err := store.Load()
	if err != nil {
		return err
	}
	client, err := localapi.New(state.LocalBaseURL, relay.DefaultMaxResponseBytes)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	payload, err := client.Models(ctx)
	if err != nil {
		return err
	}
	var pretty interface{}
	if err := json.Unmarshal(payload, &pretty); err != nil {
		return err
	}
	encoder := json.NewEncoder(application.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(pretty)
}

func (application *App) status(arguments []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	state, _, err := store.Load()
	if err != nil {
		return err
	}
	status, statusErr := store.LoadStatus()
	if statusErr != nil && !errors.Is(statusErr, os.ErrNotExist) {
		return statusErr
	}
	credentialStore := "file"
	if state.TokenInKeyring {
		credentialStore = "secret-service"
	}
	result := map[string]interface{}{
		"paired":           true,
		"device_id":        state.DeviceID,
		"device_name":      state.DeviceName,
		"school_origin":    state.SchoolOrigin,
		"local_base_url":   state.LocalBaseURL,
		"paired_at":        state.PairedAt,
		"credential_store": credentialStore,
		"runtime":          status,
	}
	encoder := json.NewEncoder(application.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func (application *App) unpair(arguments []string) error {
	flags := flag.NewFlagSet("unpair", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	localOnly := flags.Bool("local-only", false, "remove local credentials even if the school cannot be reached")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	state, token, err := store.Load()
	if err != nil {
		if *localOnly {
			if deleteErr := store.Delete(); deleteErr != nil {
				return deleteErr
			}
			fmt.Fprintln(application.Stdout, "Invalid local companion state removed; revoke any remaining device in the EduKod admin panel.")
			return nil
		}
		return err
	}
	if !*localOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := gateway.New().Revoke(ctx, state.SchoolOrigin, token); err != nil {
			return fmt.Errorf("revoke device at school: %w (use --local-only only after revoking it in the admin panel)", err)
		}
	}
	if err := store.Delete(); err != nil {
		return err
	}
	fmt.Fprintln(application.Stdout, "Local companion credentials removed.")
	return nil
}

func (application *App) run(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(application.Stderr)
	configDirectory := commonConfigFlag(flags)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	store, err := newStore(*configDirectory)
	if err != nil {
		return err
	}
	state, token, err := store.Load()
	if err != nil {
		return err
	}
	schoolOrigin, err := gateway.ValidateSchoolOrigin(state.SchoolOrigin)
	if err != nil {
		return fmt.Errorf("stored school origin is invalid: %w", err)
	}
	if _, err := gateway.ValidateWebSocketURL(state.WebSocketURL, schoolOrigin); err != nil {
		return fmt.Errorf("stored gateway URL is invalid: %w", err)
	}
	localClient, err := localapi.New(state.LocalBaseURL, relay.DefaultMaxResponseBytes)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	backoff := time.Second
	var lastConnectedAt time.Time
	for {
		if ctx.Err() != nil {
			_ = store.SaveStatus(config.RuntimeStatus{Connected: false, DeviceID: state.DeviceID, LastConnectedAt: lastConnectedAt})
			return nil
		}
		connection, dialErr := (wsclient.Dialer{
			HandshakeTimeout: 15 * time.Second,
			ReadTimeout:      75 * time.Second,
			// The same WebSocket limit applies in both directions. Gateway
			// requests are still independently capped at MaxRequestBytes by the
			// relay, while model responses may legitimately be larger.
			MaxMessageBytes: relay.DefaultMaxResponseBytes + 4096,
		}).Dial(ctx, state.WebSocketURL, token)
		if dialErr == nil {
			now := time.Now().UTC()
			lastConnectedAt = now
			_ = store.SaveStatus(config.RuntimeStatus{
				Connected:       true,
				DeviceID:        state.DeviceID,
				LastConnectedAt: now,
			})
			fmt.Fprintf(application.Stdout, "Connected to %s as %s.\n", state.SchoolOrigin, state.DeviceName)
			dialErr = relay.Serve(ctx, connection, localClient, relay.Options{
				DeviceID:   state.DeviceID,
				DeviceName: state.DeviceName,
			})
		}
		if ctx.Err() != nil {
			_ = store.SaveStatus(config.RuntimeStatus{Connected: false, DeviceID: state.DeviceID, LastConnectedAt: lastConnectedAt})
			return nil
		}
		message := safeError(dialErr)
		_ = store.SaveStatus(config.RuntimeStatus{Connected: false, DeviceID: state.DeviceID, LastConnectedAt: lastConnectedAt, LastError: message})
		fmt.Fprintf(application.Stderr, "Companion connection unavailable: %s; reconnecting.\n", message)
		delay := jitter(backoff)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if !lastConnectedAt.IsZero() && time.Since(lastConnectedAt) >= 30*time.Second {
			backoff = time.Second
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func safeError(err error) string {
	if err == nil {
		return "connection closed"
	}
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	message = strings.ReplaceAll(message, "\r", " ")
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return time.Second
	}
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return base
	}
	fraction := float64(binary.BigEndian.Uint64(randomBytes[:])) / float64(^uint64(0))
	return time.Duration(float64(base) * (0.75 + fraction*0.5))
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "EduKod AI workstation"
	}
	return name
}
