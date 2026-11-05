package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/demoports"
	"github.com/Alex-Zechariah-02/apiward/internal/services"
)

const warmupPairs = 3

type latency struct {
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
}
type measurement struct {
	At               time.Time `json:"at"`
	Account          string    `json:"account"`
	Samples          int       `json:"samples"`
	WarmupPairs      int       `json:"warmup_pairs"`
	Concurrency      int       `json:"concurrency"`
	Direct           latency   `json:"direct"`
	Gateway          latency   `json:"gateway"`
	AddedMeanMS      float64   `json:"added_mean_ms"`
	DirectSamplesMS  []float64 `json:"direct_samples_ms"`
	GatewaySamplesMS []float64 `json:"gateway_samples_ms"`
	Clock            string    `json:"clock"`
	ClockFrequencyHz float64   `json:"clock_frequency_hz"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "measure:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("measure", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	samples := flags.Int("samples", 30, "measured request pairs (1 to 50)")
	account := flags.String("account", "cust-001", "seeded customer, once per quota window")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *samples < 1 || *samples > 50 {
		return errors.New("use 1 to 50 samples and no positional arguments")
	}
	email := ""
	for _, user := range services.Users() {
		if user.ID == *account && slices.Contains(user.Roles, "CUSTOMER") {
			email = user.Email
		}
	}
	if email == "" {
		return errors.New("choose cust-001, cust-002 or cust-003")
	}
	data, err := demoports.Offset("127.0.0.2:9480")
	if err != nil {
		return err
	}
	orders, err := demoports.Offset("127.0.0.1:9502")
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 2 * time.Second, MaxIdleConns: 2, MaxIdleConnsPerHost: 1, IdleConnTimeout: 10 * time.Second}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	credentials, _ := json.Marshal(map[string]string{"email": email, "password": "FetchDemo2026!"})
	request, err := http.NewRequest("POST", "http://"+data+"/accounts/login", bytes.NewReader(credentials))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.GetBody = nil
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Accounts login did not answer: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	response.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	if readErr != nil || len(body) > 65536 || response.StatusCode != 200 || json.Unmarshal(body, &login) != nil || login.Token == "" {
		return errors.New("Accounts login failed; no measurement emitted")
	}
	result, err := measure(client, "http://"+orders, "http://"+data, *account, login.Token, *samples)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func measure(client *http.Client, direct, gateway, subject, token string, samples int) (measurement, error) {
	if samples < 1 || samples > 50 {
		return measurement{}, errors.New("sample count is outside 1 to 50")
	}
	clock, err := newSampleClock()
	if err != nil {
		return measurement{}, err
	}
	directTimes, gatewayTimes := []float64{}, []float64{}
	for pair := 0; pair < samples+warmupPairs; pair++ {
		var directMS, gatewayMS float64
		var directBalance, gatewayBalance int64
		var err error
		// Alternate which side goes first to reduce a consistent ordering bias.
		if pair%2 == 0 {
			directMS, directBalance, err = wallet(client, direct, subject, token, false, clock)
			if err == nil {
				gatewayMS, gatewayBalance, err = wallet(client, gateway, subject, token, true, clock)
			}
		} else {
			gatewayMS, gatewayBalance, err = wallet(client, gateway, subject, token, true, clock)
			if err == nil {
				directMS, directBalance, err = wallet(client, direct, subject, token, false, clock)
			}
		}
		if err != nil {
			return measurement{}, err
		}
		if directBalance != gatewayBalance {
			return measurement{}, errors.New("wallet changed or answers differ; no measurement emitted")
		}
		if pair >= warmupPairs {
			directTimes = append(directTimes, directMS)
			gatewayTimes = append(gatewayTimes, gatewayMS)
		}
	}
	result := measurement{At: time.Now().UTC(), Account: subject, Samples: samples, WarmupPairs: warmupPairs, Concurrency: 1, Direct: summarize(directTimes), Gateway: summarize(gatewayTimes)}
	result.AddedMeanMS = result.Gateway.MeanMS - result.Direct.MeanMS
	result.DirectSamplesMS = directTimes
	result.GatewaySamplesMS = gatewayTimes
	result.Clock = clock.name
	result.ClockFrequencyHz = clock.frequency
	return result, nil
}

func wallet(client *http.Client, origin, subject, token string, throughGateway bool, clock sampleClock) (float64, int64, error) {
	request, err := http.NewRequest("GET", origin+"/orders/wallet", nil)
	if err != nil {
		return 0, 0, err
	}
	if throughGateway {
		request.Header.Set("Authorization", "Bearer "+token)
	} else {
		request.Header.Set("X-Gateway-Subject", subject)
		request.Header.Set("X-Gateway-Roles", "CUSTOMER")
	}
	started, err := clock.read()
	if err != nil {
		return 0, 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, 0, fmt.Errorf("wallet request did not answer: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	closeErr := response.Body.Close()
	ended, clockErr := clock.read()
	if clockErr != nil {
		return 0, 0, clockErr
	}
	if ended <= started {
		return 0, 0, errors.New("measurement clock did not advance; no measurement emitted")
	}
	elapsed := float64(ended-started) * 1000 / clock.frequency
	if readErr != nil || closeErr != nil || len(body) > 4096 || response.StatusCode != 200 || response.Header.Get("X-Service") != "orders" || throughGateway && response.Header.Get("X-Request-Id") == "" {
		return 0, 0, fmt.Errorf("wallet request failed or had an invalid service response (status %d); no measurement emitted", response.StatusCode)
	}
	var reply struct {
		Balance *int64 `json:"balance_sen"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reply) != nil || reply.Balance == nil {
		return 0, 0, errors.New("wallet answer has an invalid shape; no measurement emitted")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return 0, 0, errors.New("wallet answer has trailing data; no measurement emitted")
	}
	return elapsed, *reply.Balance, nil
}

func summarize(values []float64) latency {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return latency{MeanMS: sum / float64(len(values)), P50MS: ordered[int(math.Ceil(float64(len(values))*.50))-1], P95MS: ordered[int(math.Ceil(float64(len(values))*.95))-1]}
}
