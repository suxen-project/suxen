// Command suxenctl administers a running suxen server and transfers Raw artifacts.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/rawpath"
)

type client struct {
	baseURL string
	token   string
	http    *http.Client
	ctx     context.Context
}

// version identifies this build. Release builds replace "dev" through linker flags.
var version = "dev"

func main() {
	global := flag.NewFlagSet("suxenctl", flag.ExitOnError)
	serverURL := global.String("url", env("SUXEN_URL", "http://localhost:8080"), "suxen base URL")
	token := global.String("token", os.Getenv("SUXEN_TOKEN"), "Bearer API token")
	global.Usage = usage
	_ = global.Parse(os.Args[1:])
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api := &client{
		baseURL: strings.TrimRight(*serverURL, "/"),
		token:   *token,
		http:    newHTTPClient(),
		ctx:     ctx,
	}
	if err := dispatch(api, global.Args()); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "suxenctl:", err)
		os.Exit(1)
	}
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: checkRedirect}
}

// Keep credentials and replayable request bodies on the configured origin.
// Checking only the initial URL would let a redirect downgrade provisioning
// secrets to HTTP or send them to another server.
func checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	origin := via[0].URL
	target := request.URL
	if !strings.EqualFold(origin.Scheme, target.Scheme) ||
		!strings.EqualFold(origin.Hostname(), target.Hostname()) ||
		originPort(origin) != originPort(target) {
		return errors.New("refusing redirect outside the configured server origin")
	}
	// Some Go versions drop sensitive headers when only hostname casing
	// changes. Restore our credential only after verifying the full origin.
	if authorization := via[0].Header.Get("Authorization"); authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return nil
}

func originPort(address *url.URL) string {
	if port := address.Port(); port != "" {
		return port
	}
	if strings.EqualFold(address.Scheme, "https") {
		return "443"
	}
	return "80"
}

func dispatch(api *client, arguments []string) error {
	if len(arguments) == 0 {
		usage()
		return errors.New("command is required")
	}

	switch arguments[0] {
	case "help":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl help")
		}
		usage()
		return nil
	case "version":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl version")
		}
		fmt.Println(version)
		return nil
	case "apply":
		return applyCommand(api, arguments[1:])
	case "blob-store":
		return blobStoreCommand(api, arguments[1:])
	case "oidc-provider":
		return oidcProviderCommand(api, arguments[1:])
	case "repo":
		return repositoryCommand(api, arguments[1:])
	case "attribute":
		return assetAttributeCommand(api, arguments[1:])
	case "raw":
		return rawCommand(api, arguments[1:])
	case "role":
		return roleCommand(api, arguments[1:])
	case "user":
		return userCommand(api, arguments[1:])
	case "classification":
		return classificationCommand(api, arguments[1:])
	case "cleanup-policy":
		return cleanupPolicyCommand(api, arguments[1:])
	case "cleanup":
		return cleanupCommand(api, arguments[1:])
	case "task":
		return taskCommand(api, arguments[1:])
	case "webhook":
		return webhookCommand(api, arguments[1:])
	case "download-gate":
		return downloadGateCommand(api, arguments[1:])
	case "trust-policy":
		return trustPolicyCommand(api, arguments[1:])
	case "verify":
		return verificationCommand(api, arguments[1:])
	case "stats":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl stats")
		}
		return api.printJSON(http.MethodGet, "/api/v1/stats", nil)
	case "whoami":
		return whoamiCommand(api, arguments[1:])
	case "gc":
		return garbageCollectionCommand(api, arguments[1:])
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func whoamiCommand(api *client, arguments []string) error {
	if len(arguments) != 0 {
		return errors.New("usage: suxenctl whoami")
	}
	return api.printJSON(http.MethodGet, "/api/v1/whoami", nil)
}

func applyCommand(api *client, arguments []string) error {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	fileName := flags.String("f", "", "desired-state YAML or JSON file")
	dryRun := flags.Bool("dry-run", false, "report changes without applying them")
	prune := flags.Bool("prune", false, "delete managed resources omitted from the document")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fileName == "" {
		return errors.New("usage: suxenctl apply -f FILE [--dry-run] [--prune]")
	}
	if strings.TrimSpace(api.token) == "" {
		return errors.New("suxenctl apply requires an authentication token")
	}
	if err := requireSecureProvisionTransport(api.baseURL); err != nil {
		return err
	}

	reader, closeReader, err := openProvisionInput(*fileName)
	if err != nil {
		return err
	}
	defer closeReader()
	document, err := provision.Parse(reader)
	if err != nil {
		return err
	}
	document, err = provision.ResolveSecrets(document, provision.EnvironmentResolver{})
	if err != nil {
		return err
	}
	payload, err := provision.MarshalResolved(document)
	if err != nil {
		return err
	}
	query := url.Values{}
	query.Set("dryRun", strconv.FormatBool(*dryRun))
	query.Set("prune", strconv.FormatBool(*prune))
	headers := http.Header{"Content-Type": []string{"application/json"}}
	response, err := api.doWithHeaders(http.MethodPost, "/api/v1/provision?"+query.Encode(), bytes.NewReader(payload), headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read provisioning report: %w", err)
	}
	if _, err := os.Stdout.Write(contents); err != nil {
		return fmt.Errorf("print provisioning report: %w", err)
	}
	var report provision.Report
	if err := json.Unmarshal(contents, &report); err != nil {
		return fmt.Errorf("decode provisioning report: %w", err)
	}
	if report.Failed() {
		failed := 0
		for _, result := range report.Results {
			if result.Status == provision.StatusFailed {
				failed++
			}
		}
		return fmt.Errorf("provisioning failed for %d resource(s)", failed)
	}
	return nil
}

func openProvisionInput(fileName string) (io.Reader, func(), error) {
	if fileName == "-" {
		return os.Stdin, func() {}, nil
	}
	file, err := os.Open(fileName)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open provisioning file: %w", err)
	}
	return file, func() { _ = file.Close() }, nil
}

func requireSecureProvisionTransport(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse server URL: %w", err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if parsed.Scheme == "http" && (host == "localhost" || address != nil && address.IsLoopback()) {
		return nil
	}
	return errors.New("provisioning secrets require HTTPS, except for loopback server URLs")
}

func blobStoreCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("blob-store command requires list, get, create, update, or delete")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl blob-store list")
		}
		return api.printJSON(http.MethodGet, "/api/v1/blob-stores", nil)
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl blob-store get NAME")
		}
		return api.printJSON(
			http.MethodGet,
			"/api/v1/blob-stores/"+url.PathEscape(arguments[1]),
			nil,
		)
	case "create":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl blob-store create JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPost, "/api/v1/blob-stores", payload)
	case "update":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl blob-store update NAME JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		return api.printJSON(
			http.MethodPut,
			"/api/v1/blob-stores/"+url.PathEscape(arguments[1]),
			payload,
		)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl blob-store delete NAME")
		}
		return api.discard(
			http.MethodDelete,
			"/api/v1/blob-stores/"+url.PathEscape(arguments[1]),
			nil,
		)
	default:
		return fmt.Errorf("unknown blob-store command %q", arguments[0])
	}
}

func oidcProviderCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New(
			"oidc-provider command requires list, get, create, update, or delete",
		)
	}

	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl oidc-provider list")
		}
		return api.printCollection("/api/v1/oidc-providers", 0)
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl oidc-provider get NAME")
		}
		return api.printJSON(
			http.MethodGet,
			"/api/v1/oidc-providers/"+url.PathEscape(arguments[1]),
			nil,
		)
	case "create":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl oidc-provider create JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPost, "/api/v1/oidc-providers", payload)
	case "update":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl oidc-provider update NAME JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		return api.printJSON(
			http.MethodPut,
			"/api/v1/oidc-providers/"+url.PathEscape(arguments[1]),
			payload,
		)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl oidc-provider delete NAME")
		}
		return api.discard(
			http.MethodDelete,
			"/api/v1/oidc-providers/"+url.PathEscape(arguments[1]),
			nil,
		)
	default:
		return fmt.Errorf("unknown oidc-provider command %q", arguments[0])
	}
}

func assetAttributeCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("attribute command requires get, set, or delete")
	}

	switch arguments[0] {
	case "get":
		if len(arguments) != 4 {
			return errors.New(
				"usage: suxenctl attribute get REPOSITORY ASSET_ID NAMESPACE",
			)
		}
		requestPath, err := assetAttributePath(arguments[1], arguments[2], arguments[3])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		flags := flag.NewFlagSet("attribute set", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		ifMatch := flags.String("if-match", "", "current asset digest")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		values := flags.Args()
		if len(values) != 4 || strings.TrimSpace(*ifMatch) == "" {
			return errors.New(
				"usage: suxenctl attribute set --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE JSON_FILE",
			)
		}
		requestPath, err := assetAttributePath(values[0], values[1], values[2])
		if err != nil {
			return err
		}
		payload, err := readJSONFile(values[3])
		if err != nil {
			return err
		}
		headers := make(http.Header)
		headers.Set("If-Match", strings.TrimSpace(*ifMatch))
		return api.printJSONWithHeaders(http.MethodPut, requestPath, payload, headers)
	case "delete":
		flags := flag.NewFlagSet("attribute delete", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		ifMatch := flags.String("if-match", "", "current asset digest")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		values := flags.Args()
		if len(values) != 3 || strings.TrimSpace(*ifMatch) == "" {
			return errors.New(
				"usage: suxenctl attribute delete --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE",
			)
		}
		requestPath, err := assetAttributePath(values[0], values[1], values[2])
		if err != nil {
			return err
		}
		headers := make(http.Header)
		headers.Set("If-Match", strings.TrimSpace(*ifMatch))
		return api.discardWithHeaders(http.MethodDelete, requestPath, nil, headers)
	default:
		return fmt.Errorf("unknown attribute command %q", arguments[0])
	}
}

func assetAttributePath(repositoryName, assetIDValue, namespace string) (string, error) {
	assetID, err := strconv.ParseInt(assetIDValue, 10, 64)
	if err != nil || assetID <= 0 {
		return "", fmt.Errorf("invalid asset ID %q", assetIDValue)
	}
	if strings.TrimSpace(namespace) == "" {
		return "", errors.New("attribute namespace is required")
	}
	return fmt.Sprintf(
		"/api/v1/repositories/%s/assets/%d/attributes/%s",
		url.PathEscape(repositoryName),
		assetID,
		url.PathEscape(namespace),
	), nil
}

func webhookCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("webhook command requires list, get, create, apply, update, delete, or deliveries")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl webhook list")
		}
		return api.printCollection("/api/v1/webhooks", 0)
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl webhook get NAME")
		}
		return api.printJSON(
			http.MethodGet,
			"/api/v1/webhooks/"+url.PathEscape(arguments[1]),
			nil,
		)
	case "create":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl webhook create JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPost, "/api/v1/webhooks", payload)
	case "apply":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl webhook apply NAME JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		return api.printJSON(
			http.MethodPut,
			"/api/v1/webhooks/"+url.PathEscape(arguments[1]),
			payload,
		)
	case "update":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl webhook update NAME JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		return api.printJSON(
			http.MethodPut,
			"/api/v1/webhooks/"+url.PathEscape(arguments[1]),
			payload,
		)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl webhook delete NAME")
		}
		return api.discard(
			http.MethodDelete,
			"/api/v1/webhooks/"+url.PathEscape(arguments[1]),
			nil,
		)
	case "deliveries":
		flags := flag.NewFlagSet("webhook deliveries", flag.ContinueOnError)
		limit := flags.Int("limit", 100, "maximum number of deliveries")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: suxenctl webhook deliveries [--limit N] NAME")
		}
		if *limit < 1 || *limit > 1000 {
			return errors.New("delivery limit must be between 1 and 1000")
		}
		requestPath := "/api/v1/webhooks/" + url.PathEscape(flags.Arg(0)) +
			"/deliveries?limit=" + url.QueryEscape(strconv.Itoa(*limit))
		return api.printCollection(requestPath, *limit)
	default:
		return fmt.Errorf("unknown webhook command %q", arguments[0])
	}
}

func downloadGateCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("download-gate command requires get, set, or delete")
	}
	switch arguments[0] {
	case "defaults":
		return downloadGateDefaultsCommand(api, arguments[1:])
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl download-gate get REPOSITORY")
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) +
			"/download-gate"
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl download-gate set REPOSITORY JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) +
			"/download-gate"
		return api.printJSON(http.MethodPut, requestPath, payload)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl download-gate delete REPOSITORY")
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) +
			"/download-gate"
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown download-gate command %q", arguments[0])
	}
}

func downloadGateDefaultsCommand(api *client, arguments []string) error {
	const requestPath = "/api/v1/download-gate-defaults"
	if len(arguments) == 0 {
		return errors.New("download-gate defaults command requires get, set, or delete")
	}
	switch arguments[0] {
	case "get":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl download-gate defaults get")
		}
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl download-gate defaults set JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPut, requestPath, payload)
	case "delete":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl download-gate defaults delete")
		}
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown download-gate defaults command %q", arguments[0])
	}
}

func classificationCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("classification command requires get or set")
	}
	switch arguments[0] {
	case "defaults":
		return classificationDefaultsCommand(api, arguments[1:])
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl classification get REPOSITORY")
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) +
			"/classification"
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl classification set REPOSITORY JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) +
			"/classification"
		return api.printJSON(http.MethodPut, requestPath, payload)
	default:
		return fmt.Errorf("unknown classification command %q", arguments[0])
	}
}

func classificationDefaultsCommand(api *client, arguments []string) error {
	const requestPath = "/api/v1/classification-defaults"
	if len(arguments) == 0 {
		return errors.New("classification defaults command requires get, set, or delete")
	}
	switch arguments[0] {
	case "get":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl classification defaults get")
		}
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl classification defaults set JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPut, requestPath, payload)
	case "delete":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl classification defaults delete")
		}
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown classification defaults command %q", arguments[0])
	}
}

func cleanupPolicyCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("cleanup-policy command requires list, create, update, or delete")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl cleanup-policy list")
		}
		return api.printCollection("/api/v1/cleanup-policies", 0)
	case "create":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl cleanup-policy create JSON_FILE")
		}
		payload, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPost, "/api/v1/cleanup-policies", payload)
	case "update":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl cleanup-policy update NAME JSON_FILE")
		}
		payload, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		requestPath := "/api/v1/cleanup-policies/" + url.PathEscape(arguments[1])
		return api.printJSON(http.MethodPut, requestPath, payload)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl cleanup-policy delete NAME")
		}
		requestPath := "/api/v1/cleanup-policies/" + url.PathEscape(arguments[1])
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown cleanup-policy command %q", arguments[0])
	}
}

func cleanupCommand(api *client, arguments []string) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	apply := flags.Bool("apply", false, "apply deletions instead of previewing them")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: suxenctl cleanup [--apply] REPOSITORY POLICY")
	}
	query := url.Values{
		"policy": {flags.Arg(1)},
		"dryRun": {fmt.Sprint(!*apply)},
	}
	requestPath := "/api/v1/repositories/" + url.PathEscape(flags.Arg(0)) +
		"/cleanup?" + query.Encode()
	return api.printJSON(http.MethodPost, requestPath, nil)
}

func taskCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("task command requires list, get, or leader")
	}
	switch arguments[0] {
	case "list":
		flags := flag.NewFlagSet("task list", flag.ContinueOnError)
		limit := flags.Int("limit", 100, "maximum number of tasks")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("usage: suxenctl task list [--limit N]")
		}
		if *limit < 1 || *limit > 1000 {
			return errors.New("task limit must be between 1 and 1000")
		}
		return api.printCollection("/api/v1/tasks", *limit)
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl task get ID")
		}
		return api.printJSON(
			http.MethodGet,
			"/api/v1/tasks/"+url.PathEscape(arguments[1]),
			nil,
		)
	case "leader":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl task leader")
		}
		return api.printJSON(http.MethodGet, "/api/v1/tasks/leader", nil)
	default:
		return fmt.Errorf("unknown task command %q", arguments[0])
	}
}

func roleCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("role command requires list, create, or delete")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl role list")
		}
		return api.printCollection("/api/v1/roles", 0)
	case "create":
		flags := flag.NewFlagSet("role create", flag.ContinueOnError)
		description := flags.String("description", "", "role description")
		privileges := flags.String("privileges", "", "comma-separated privileges")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: suxenctl role create [flags] NAME")
		}
		payload := map[string]any{
			"name":        flags.Arg(0),
			"description": *description,
			"privileges":  commaSeparated(*privileges),
		}
		return api.printJSON(http.MethodPost, "/api/v1/roles", payload)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl role delete NAME")
		}
		return api.discard(http.MethodDelete, "/api/v1/roles/"+url.PathEscape(arguments[1]), nil)
	default:
		return fmt.Errorf("unknown role command %q", arguments[0])
	}
}

func userCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("user command requires list, create, roles, token, tokens, or revoke")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl user list")
		}
		return api.printCollection("/api/v1/users", 0)
	case "create":
		return createUser(api, arguments[1:])
	case "roles":
		return userRoles(api, arguments[1:])
	case "token":
		return createUserToken(api, arguments[1:])
	case "tokens":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl user tokens USER")
		}
		requestPath := "/api/v1/users/" + url.PathEscape(arguments[1]) + "/tokens"
		return api.printCollection(requestPath, 0)
	case "revoke":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl user revoke USER TOKEN_ID")
		}
		requestPath := "/api/v1/users/" + url.PathEscape(arguments[1]) +
			"/tokens/" + url.PathEscape(arguments[2])
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown user command %q", arguments[0])
	}
}

func createUser(api *client, arguments []string) error {
	flags := flag.NewFlagSet("user create", flag.ContinueOnError)
	password := flags.String("password", "", "initial password")
	roles := flags.String("roles", "", "comma-separated roles")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 || *password == "" {
		return errors.New("usage: suxenctl user create --password PASSWORD [flags] NAME")
	}
	payload := map[string]any{
		"username": flags.Arg(0),
		"password": *password,
		"roles":    commaSeparated(*roles),
	}
	return api.printJSON(http.MethodPost, "/api/v1/users", payload)
}

func userRoles(api *client, arguments []string) error {
	flags := flag.NewFlagSet("user roles", flag.ContinueOnError)
	roles := flags.String("set", "", "replace assignments with comma-separated roles")
	clear := flags.Bool("clear", false, "remove all role assignments")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: suxenctl user roles [--set ROLE,... | --clear] USER")
	}
	setProvided := false
	flags.Visit(func(flag *flag.Flag) {
		setProvided = setProvided || flag.Name == "set"
	})
	if *clear && setProvided {
		return errors.New("--set and --clear cannot be used together")
	}
	parsedRoles := commaSeparated(*roles)
	if setProvided && len(parsedRoles) == 0 {
		return errors.New("--set requires at least one role; use --clear to remove all roles")
	}
	requestPath := "/api/v1/users/" + url.PathEscape(flags.Arg(0)) + "/roles"
	if !setProvided && !*clear {
		return api.printJSON(http.MethodGet, requestPath, nil)
	}
	return api.printJSON(
		http.MethodPut,
		requestPath,
		map[string]any{"roles": parsedRoles},
	)
}

func createUserToken(api *client, arguments []string) error {
	flags := flag.NewFlagSet("user token", flag.ContinueOnError)
	name := flags.String("name", "api", "token name")
	scopes := flags.String("scopes", "", "comma-separated maximum privileges")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: suxenctl user token [flags] USER")
	}
	requestPath := "/api/v1/users/" + url.PathEscape(flags.Arg(0)) + "/tokens"
	return api.printJSON(http.MethodPost, requestPath, map[string]any{
		"name":   *name,
		"scopes": commaSeparated(*scopes),
	})
}

func garbageCollectionCommand(api *client, arguments []string) error {
	flags := flag.NewFlagSet("gc", flag.ContinueOnError)
	apply := flags.Bool("apply", false, "delete eligible unreferenced blobs")
	grace := flags.String("grace", "24h", "minimum age of unreferenced blobs")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: suxenctl gc [--apply] [--grace DURATION]")
	}
	values := url.Values{
		"dryRun": {fmt.Sprint(!*apply)},
		"grace":  {*grace},
	}
	return api.printJSON(http.MethodPost, "/api/v1/gc?"+values.Encode(), nil)
}

func repositoryCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("repo command requires list, create, assets, components, or delete")
	}

	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl repo list")
		}
		return api.printCollection("/api/v1/repositories", 0)
	case "create":
		return createRepository(api, arguments[1:])
	case "assets":
		if len(arguments) < 2 || len(arguments) > 3 {
			return errors.New("usage: suxenctl repo assets NAME [PREFIX]")
		}
		values := url.Values{}
		if len(arguments) == 3 {
			values.Set("prefix", arguments[2])
		}
		requestPath := "/api/v1/repositories/" + url.PathEscape(arguments[1]) + "/assets"
		if encoded := values.Encode(); encoded != "" {
			requestPath += "?" + encoded
		}
		return api.printCollection(requestPath, 0)
	case "components":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl repo components NAME")
		}
		return api.printCollection("/api/v1/repositories/"+url.PathEscape(arguments[1])+"/components", 0)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl repo delete NAME")
		}
		return api.discard(http.MethodDelete, "/api/v1/repositories/"+url.PathEscape(arguments[1]), nil)
	default:
		return fmt.Errorf("unknown repo command %q", arguments[0])
	}
}

func createRepository(api *client, arguments []string) error {
	flags := flag.NewFlagSet("repo create", flag.ContinueOnError)
	format := flags.String("format", "raw", "registered repository format name")
	repositoryType := flags.String("type", "hosted", "repository type: hosted, proxy, or group")
	upstream := flags.String("upstream", "", "proxy upstream URL")
	members := flags.String("members", "", "comma-separated group members")
	blobStore := flags.String("blob-store", "default", "named blob store")
	formatConfig := flags.String("format-config", "", `format-owned settings as JSON (e.g. {"versionPolicy":"release"})`)
	allowOverwrite := flags.Bool("allow-overwrite", false, "allow replacing hosted assets (omitted: format default)")
	components := &rawComponentFlags{}
	flags.Var(components, "component", "Raw component pattern with name and version groups (repeatable, in match order)")
	flags.Var(rawComponentAnchorFlag{components}, "component-anchor", "anchor pattern for the preceding --component")
	hosts := flags.String("hosts", "", "comma-separated OCI registry hostnames")
	ports := flags.String("ports", "", "comma-separated extra OCI listen ports")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: suxenctl repo create [flags] NAME")
	}

	payload := map[string]any{
		"name":      flags.Arg(0),
		"format":    *format,
		"type":      *repositoryType,
		"blobStore": *blobStore,
		"upstream":  *upstream,
	}
	if *members != "" {
		payload["members"] = strings.Split(*members, ",")
	}
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "allow-overwrite" {
			payload["allowOverwrite"] = *allowOverwrite
		}
	})
	if *formatConfig != "" {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(*formatConfig), &parsed); err != nil {
			return fmt.Errorf("parse --format-config: %w", err)
		}
		payload["formatConfig"] = parsed
	}
	if len(components.rules) > 0 {
		config, _ := payload["formatConfig"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		if _, present := config["components"]; present {
			return errors.New("--component cannot be combined with components in --format-config")
		}
		config["components"] = components.rules
		payload["formatConfig"] = config
	}
	if *hosts != "" || *ports != "" {
		endpoints := map[string]any{}
		if *hosts != "" {
			endpoints["hosts"] = strings.Split(*hosts, ",")
		}
		if *ports != "" {
			parsedPorts := make([]int, 0)
			for _, value := range strings.Split(*ports, ",") {
				port, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return fmt.Errorf("parse --ports: %w", err)
				}
				parsedPorts = append(parsedPorts, port)
			}
			endpoints["ports"] = parsedPorts
		}
		payload["endpoints"] = endpoints
	}
	return api.printJSON(http.MethodPost, "/api/v1/repositories", payload)
}

// rawComponentFlags collects --component values in command-line order, so the
// first pattern given is the first one matched.
type rawComponentFlags struct {
	rules []map[string]any
}

func (components *rawComponentFlags) String() string { return "" }

func (components *rawComponentFlags) Set(pattern string) error {
	components.rules = append(components.rules, map[string]any{"pattern": pattern})
	return nil
}

// rawComponentAnchorFlag attaches --component-anchor to the preceding pattern.
type rawComponentAnchorFlag struct {
	components *rawComponentFlags
}

func (rawComponentAnchorFlag) String() string { return "" }

func (anchor rawComponentAnchorFlag) Set(value string) error {
	rules := anchor.components.rules
	if len(rules) == 0 {
		return errors.New("--component-anchor must follow a --component")
	}
	if _, present := rules[len(rules)-1]["anchor"]; present {
		return errors.New("each --component accepts one --component-anchor")
	}
	rules[len(rules)-1]["anchor"] = value
	return nil
}

func rawCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("raw command requires put, get, or delete")
	}

	switch arguments[0] {
	case "put":
		flags := flag.NewFlagSet("raw put", flag.ContinueOnError)
		signaturePath := flags.String("signature", "", "detached binary signature file")
		certificatePath := flags.String("certificate", "", "PEM signing certificate file")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 3 {
			return errors.New(
				"usage: suxenctl raw put [--signature FILE] [--certificate FILE] " +
					"REPOSITORY PATH FILE",
			)
		}
		requestPath, err := rawpath.URLPath(flags.Arg(0), flags.Arg(1))
		if err != nil {
			return err
		}

		headers, err := signedUploadHeaders(*signaturePath, *certificatePath)
		if err != nil {
			return err
		}
		file, err := os.Open(flags.Arg(2))
		if err != nil {
			return err
		}
		defer file.Close()
		return api.printJSONWithHeaders(
			http.MethodPut,
			requestPath,
			file,
			headers,
		)
	case "get":
		if len(arguments) != 4 {
			return errors.New("usage: suxenctl raw get REPOSITORY PATH FILE")
		}
		requestPath, err := rawpath.URLPath(arguments[1], arguments[2])
		if err != nil {
			return err
		}
		return api.download(requestPath, arguments[3])
	case "delete":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl raw delete REPOSITORY PATH")
		}
		requestPath, err := rawpath.URLPath(arguments[1], arguments[2])
		if err != nil {
			return err
		}
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown raw command %q", arguments[0])
	}
}

func signedUploadHeaders(signaturePath, certificatePath string) (http.Header, error) {
	headers := make(http.Header)
	if signaturePath != "" {
		signature, err := os.ReadFile(signaturePath)
		if err != nil {
			return nil, fmt.Errorf("read signature: %w", err)
		}
		headers.Set("X-Suxen-Signature", base64.StdEncoding.EncodeToString(signature))
	}
	if certificatePath != "" {
		certificate, err := os.ReadFile(certificatePath)
		if err != nil {
			return nil, fmt.Errorf("read certificate: %w", err)
		}
		headers.Set("X-Suxen-Certificate", base64.StdEncoding.EncodeToString(certificate))
	}
	return headers, nil
}

func trustPolicyCommand(api *client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("trust-policy command requires get, set, or delete")
	}

	switch arguments[0] {
	case "defaults":
		return trustPolicyDefaultsCommand(api, arguments[1:])
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl trust-policy get REPOSITORY")
		}
		return api.printJSON(
			http.MethodGet,
			trustPolicyPath(arguments[1]),
			nil,
		)
	case "set":
		if len(arguments) != 3 {
			return errors.New("usage: suxenctl trust-policy set REPOSITORY JSON_FILE")
		}
		policy, err := readJSONFile(arguments[2])
		if err != nil {
			return err
		}
		return api.printJSON(
			http.MethodPut,
			trustPolicyPath(arguments[1]),
			policy,
		)
	case "delete":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl trust-policy delete REPOSITORY")
		}
		return api.discard(
			http.MethodDelete,
			trustPolicyPath(arguments[1]),
			nil,
		)
	default:
		return fmt.Errorf("unknown trust-policy command %q", arguments[0])
	}
}

func trustPolicyDefaultsCommand(api *client, arguments []string) error {
	const requestPath = "/api/v1/trust-policy-defaults"
	if len(arguments) == 0 {
		return errors.New("trust-policy defaults command requires get, set, or delete")
	}
	switch arguments[0] {
	case "get":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl trust-policy defaults get")
		}
		return api.printJSON(http.MethodGet, requestPath, nil)
	case "set":
		if len(arguments) != 2 {
			return errors.New("usage: suxenctl trust-policy defaults set JSON_FILE")
		}
		policy, err := readJSONFile(arguments[1])
		if err != nil {
			return err
		}
		return api.printJSON(http.MethodPut, requestPath, policy)
	case "delete":
		if len(arguments) != 1 {
			return errors.New("usage: suxenctl trust-policy defaults delete")
		}
		return api.discard(http.MethodDelete, requestPath, nil)
	default:
		return fmt.Errorf("unknown trust-policy defaults command %q", arguments[0])
	}
}

func verificationCommand(api *client, arguments []string) error {
	if len(arguments) != 3 {
		return errors.New("usage: suxenctl verify REPOSITORY ASSET_ID JSON_FILE")
	}

	assetID, err := strconv.ParseInt(arguments[1], 10, 64)
	if err != nil || assetID <= 0 {
		return fmt.Errorf("invalid asset ID %q", arguments[1])
	}
	request, err := readJSONFile(arguments[2])
	if err != nil {
		return err
	}
	requestPath := fmt.Sprintf(
		"/api/v1/repositories/%s/assets/%d/verification",
		url.PathEscape(arguments[0]),
		assetID,
	)
	return api.printJSON(http.MethodPost, requestPath, request)
}

func trustPolicyPath(repositoryName string) string {
	return "/api/v1/repositories/" + url.PathEscape(repositoryName) + "/trust-policy"
}

func (api *client) printJSON(method, requestPath string, body any) error {
	return api.printJSONWithHeaders(method, requestPath, body, nil)
}

type commandCollectionPage struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"nextCursor"`
}

func (api *client) printCollection(requestPath string, maximumItems int) error {
	if maximumItems < 0 {
		return errors.New("collection limit cannot be negative")
	}
	parsed, err := url.Parse(requestPath)
	if err != nil {
		return fmt.Errorf("parse collection URL: %w", err)
	}
	items := make([]json.RawMessage, 0)
	seenCursors := make(map[string]struct{})
	for {
		pageLimit := 200
		if maximumItems > 0 && maximumItems-len(items) < pageLimit {
			pageLimit = maximumItems - len(items)
		}
		if pageLimit <= 0 {
			break
		}
		query := parsed.Query()
		query.Set("limit", strconv.Itoa(pageLimit))
		parsed.RawQuery = query.Encode()

		response, err := api.do(http.MethodGet, parsed.RequestURI(), nil)
		if err != nil {
			return err
		}
		var page commandCollectionPage
		decodeErr := json.NewDecoder(response.Body).Decode(&page)
		closeErr := response.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode collection page: %w", decodeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" || (maximumItems > 0 && len(items) >= maximumItems) {
			break
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return errors.New("server returned a repeated collection cursor")
		}
		seenCursors[page.NextCursor] = struct{}{}
		query = parsed.Query()
		query.Set("cursor", page.NextCursor)
		parsed.RawQuery = query.Encode()
	}
	return json.NewEncoder(os.Stdout).Encode(items)
}

func (api *client) printJSONWithHeaders(
	method string,
	requestPath string,
	body any,
	headers http.Header,
) error {
	response, err := api.doWithHeaders(method, requestPath, body, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(os.Stdout, response.Body)
	return err
}

func (api *client) discard(method, requestPath string, body any) error {
	return api.discardWithHeaders(method, requestPath, body, nil)
}

func (api *client) discardWithHeaders(
	method string,
	requestPath string,
	body any,
	headers http.Header,
) error {
	response, err := api.doWithHeaders(method, requestPath, body, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func (api *client) download(requestPath, destination string) error {
	response, err := api.do(http.MethodGet, requestPath, nil)
	if err != nil {
		return err
	}
	bodyClosed := false
	defer func() {
		if !bodyClosed {
			_ = response.Body.Close()
		}
	}()

	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".suxenctl-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, copyErr := io.Copy(file, response.Body)
	bodyClosed = true
	bodyCloseErr := response.Body.Close()
	if copyErr != nil {
		_ = file.Close()
		return copyErr
	}
	if bodyCloseErr != nil {
		_ = file.Close()
		return bodyCloseErr
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if api.ctx != nil && api.ctx.Err() != nil {
		return api.ctx.Err()
	}
	return replaceDownload(file.Name(), destination)
}

func (api *client) do(method, requestPath string, body any) (*http.Response, error) {
	return api.doWithHeaders(method, requestPath, body, nil)
}

func (api *client) doWithHeaders(
	method string,
	requestPath string,
	body any,
	headers http.Header,
) (*http.Response, error) {
	reader, contentType, err := requestBody(body)
	if err != nil {
		return nil, err
	}
	ctx := api.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, method, api.baseURL+requestPath, reader)
	if err != nil {
		return nil, err
	}
	if api.token != "" {
		request.Header.Set("Authorization", "Bearer "+api.token)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, values := range headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	response, err := api.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return response, nil
}

func requestBody(value any) (io.Reader, string, error) {
	if value == nil {
		return nil, "", nil
	}
	if reader, ok := value.(io.Reader); ok {
		return reader, "application/octet-stream", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(encoded), "application/json", nil
}

func readJSONFile(path string) (any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var value any
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("expected exactly one JSON value")
		}
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return value, nil
}

func env(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func commaSeparated(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: suxenctl [global flags] COMMAND

Global flags:
  --url URL       Suxen base URL (default: SUXEN_URL or http://localhost:8080)
  --token TOKEN   Bearer API token (default: SUXEN_TOKEN)

Commands:
  help
  version
  whoami
  apply -f FILE [--dry-run] [--prune]
  blob-store list|get NAME|create FILE|update NAME FILE|delete NAME
  oidc-provider list|get NAME|create FILE|update NAME FILE|delete NAME
  repo list
  repo create [--format FORMAT] [--type hosted|proxy|group]
              [--blob-store NAME] [--upstream URL] [--members NAME,...]
              [--hosts HOST,...] [--ports PORT,...] [--format-config JSON]
              [--component PATTERN [--component-anchor PATTERN]]... NAME
  repo assets NAME [PREFIX]
  repo components NAME
  repo delete NAME
  attribute get REPOSITORY ASSET_ID NAMESPACE
  attribute set --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE FILE
  attribute delete --if-match DIGEST REPOSITORY ASSET_ID NAMESPACE
  raw put [--signature FILE] [--certificate FILE] REPOSITORY PATH FILE
  raw get REPOSITORY PATH FILE
  raw delete REPOSITORY PATH
  role list
  role create [--description TEXT] [--privileges VALUE,...] NAME
  role delete NAME
  user list
  user create --password PASSWORD [--roles NAME,...] NAME
  user roles [--set NAME,...|--clear] NAME
  user token [--name NAME] [--scopes VALUE,...] NAME
  user tokens NAME
  user revoke NAME TOKEN_ID
  classification get REPOSITORY
  classification set REPOSITORY FILE
  classification defaults get|set FILE|delete
  cleanup-policy list|create FILE|update NAME FILE|delete NAME
  cleanup [--apply] REPOSITORY POLICY
  task list [--limit N]|get ID|leader
  webhook list|get NAME|create FILE|apply NAME FILE|update NAME FILE|delete NAME
  webhook deliveries [--limit N] NAME
  download-gate get REPOSITORY|set REPOSITORY FILE|delete REPOSITORY
  download-gate defaults get|set FILE|delete
  trust-policy get REPOSITORY|set REPOSITORY FILE|delete REPOSITORY
  trust-policy defaults get|set FILE|delete
  verify REPOSITORY ASSET_ID JSON_FILE
  gc [--apply] [--grace DURATION]
  stats`)
}
