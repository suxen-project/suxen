// Command capacity drives the bounded Suxen capacity baseline and its delayed
// object-store proxy. It intentionally uses only the standard library.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	Operation        string         `json:"operation"`
	Target           string         `json:"target"`
	Requests         int            `json:"requests"`
	Succeeded        int            `json:"succeeded"`
	PayloadBytes     int64          `json:"payloadBytes"`
	Concurrency      int            `json:"concurrency"`
	ElapsedMillis    float64        `json:"elapsedMillis"`
	RequestsPerSec   float64        `json:"requestsPerSecond"`
	PayloadMiBPerSec float64        `json:"payloadMiBPerSecond"`
	P50Millis        float64        `json:"p50Millis"`
	P95Millis        float64        `json:"p95Millis"`
	MaxMillis        float64        `json:"maxMillis"`
	Statuses         map[string]int `json:"statuses"`
}

type sample struct {
	duration time.Duration
	status   string
	bytes    int64
	err      error
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: capacity <raw|npm|delete|proxy> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "raw":
		err = runRaw(os.Args[2:])
	case "npm":
		err = runNPM(os.Args[2:])
	case "delete":
		err = runDelete(os.Args[2:])
	case "proxy":
		err = runProxy(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

type loadOptions struct {
	baseURL     string
	token       string
	count       int
	size        int
	concurrency int
	prefix      string
}

func loadFlags(name string, arguments []string, defaultSize int) (*flag.FlagSet, loadOptions, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	options := loadOptions{}
	flags.StringVar(&options.baseURL, "url", "", "Suxen base URL")
	flags.StringVar(&options.token, "token", "", "Bearer token")
	flags.IntVar(&options.count, "count", 48, "request count")
	flags.IntVar(&options.size, "size", defaultSize, "payload bytes per request")
	flags.IntVar(&options.concurrency, "concurrency", 8, "concurrent requests")
	flags.StringVar(&options.prefix, "prefix", "capacity", "unique asset/package prefix")
	if err := flags.Parse(arguments); err != nil {
		return flags, options, err
	}
	if options.baseURL == "" || options.token == "" || options.count < 1 || options.size < 1 || options.concurrency < 1 {
		return flags, options, errors.New("url, token, and positive count, size, and concurrency are required")
	}
	options.baseURL = strings.TrimRight(options.baseURL, "/")
	return flags, options, nil
}

func runRaw(arguments []string) error {
	_, options, err := loadFlags("raw", arguments, 1<<20)
	if err != nil {
		return err
	}
	return runLoad("raw-put", options, func(index int) (*http.Request, int64, error) {
		payload := deterministicPayload(options.size, index)
		path := fmt.Sprintf("%s/%06d.bin", options.prefix, index)
		request, err := http.NewRequest(http.MethodPut, options.baseURL+"/repository/raw/"+path, bytes.NewReader(payload))
		return request, int64(len(payload)), err
	})
}

func runNPM(arguments []string) error {
	_, options, err := loadFlags("npm", arguments, 8<<20)
	if err != nil {
		return err
	}
	return runLoad("npm-publish", options, func(index int) (*http.Request, int64, error) {
		name := fmt.Sprintf("%s-%06d", options.prefix, index)
		version := "1.0.0"
		file := name + "-" + version + ".tgz"
		payload := deterministicPayload(options.size, index)
		document := map[string]any{
			"name": name,
			"versions": map[string]any{version: map[string]any{
				"name": name, "version": version,
				"dist": map[string]any{},
			}},
			"_attachments": map[string]any{file: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(payload),
			}},
		}
		body, err := json.Marshal(document)
		if err != nil {
			return nil, 0, err
		}
		request, err := http.NewRequest(http.MethodPut, options.baseURL+"/repository/npm-capacity/"+name, bytes.NewReader(body))
		if request != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		return request, int64(len(body)), err
	})
}

func runDelete(arguments []string) error {
	flags := flag.NewFlagSet("delete", flag.ContinueOnError)
	baseURL := flags.String("url", "", "Suxen base URL")
	token := flags.String("token", "", "Bearer token")
	count := flags.Int("count", 48, "asset count")
	concurrency := flags.Int("concurrency", 8, "concurrent requests")
	prefix := flags.String("prefix", "capacity", "asset prefix")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	options := loadOptions{baseURL: strings.TrimRight(*baseURL, "/"), token: *token, count: *count, concurrency: *concurrency, size: 1, prefix: *prefix}
	if options.baseURL == "" || options.token == "" || options.count < 1 || options.concurrency < 1 {
		return errors.New("url, token, and positive count and concurrency are required")
	}
	return runLoad("raw-delete", options, func(index int) (*http.Request, int64, error) {
		path := fmt.Sprintf("%s/%06d.bin", options.prefix, index)
		request, err := http.NewRequest(http.MethodDelete, options.baseURL+"/repository/raw/"+path, nil)
		return request, 0, err
	})
}

func runLoad(operation string, options loadOptions, build func(int) (*http.Request, int64, error)) error {
	client := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{
		MaxIdleConns: options.concurrency * 2, MaxIdleConnsPerHost: options.concurrency,
	}}
	jobs := make(chan int)
	samples := make(chan sample, options.count)
	var workers sync.WaitGroup
	for range options.concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				request, size, err := build(index)
				if err != nil {
					samples <- sample{err: err}
					continue
				}
				request.Header.Set("Authorization", "Bearer "+options.token)
				started := time.Now()
				response, err := client.Do(request)
				duration := time.Since(started)
				if err != nil {
					samples <- sample{duration: duration, bytes: size, err: err}
					continue
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil {
					err = readErr
				} else if closeErr != nil {
					err = closeErr
				}
				samples <- sample{duration: duration, status: response.Status, bytes: size, err: err}
			}
		}()
	}
	started := time.Now()
	go func() {
		for index := range options.count {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(samples)
	}()

	durations := make([]time.Duration, 0, options.count)
	statuses := map[string]int{}
	var succeeded int
	var transferred int64
	var failures []error
	for observed := range samples {
		durations = append(durations, observed.duration)
		transferred += observed.bytes
		if observed.status != "" {
			statuses[observed.status]++
		}
		if observed.err != nil {
			failures = append(failures, observed.err)
		} else if strings.HasPrefix(observed.status, "2") {
			succeeded++
		} else {
			failures = append(failures, errors.New(observed.status))
		}
	}
	elapsed := time.Since(started)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	report := result{
		Operation: operation, Target: options.baseURL, Requests: options.count,
		Succeeded: succeeded, PayloadBytes: transferred, Concurrency: options.concurrency,
		ElapsedMillis: millis(elapsed), RequestsPerSec: float64(options.count) / elapsed.Seconds(),
		PayloadMiBPerSec: float64(transferred) / (1 << 20) / elapsed.Seconds(),
		P50Millis:        millis(percentile(durations, 0.50)), P95Millis: millis(percentile(durations, 0.95)),
		MaxMillis: millis(durations[len(durations)-1]), Statuses: statuses,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if len(failures) != 0 {
		return fmt.Errorf("%d requests failed (first: %v)", len(failures), failures[0])
	}
	return nil
}

func deterministicPayload(size, index int) []byte {
	payload := bytes.Repeat([]byte{byte(index*31 + 17)}, size)
	if len(payload) >= 8 {
		binary.LittleEndian.PutUint64(payload[:8], uint64(index))
	}
	return payload
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1)*fraction + 0.5)
	return values[index]
}

func millis(duration time.Duration) float64 { return float64(duration) / float64(time.Millisecond) }

func runProxy(arguments []string) error {
	flags := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listen := flags.String("listen", ":8080", "listen address")
	targetValue := flags.String("target", "", "upstream URL")
	delay := flags.Duration("delay", 5*time.Millisecond, "delay before each upstream request")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	target, err := url.Parse(*targetValue)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return errors.New("target must be an absolute URL")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		timer := time.NewTimer(*delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		proxy.ServeHTTP(writer, request)
	})
	log.Printf("delayed proxy listening on %s for %s with %s/request", *listen, target, *delay)
	return http.ListenAndServe(*listen, handler)
}
