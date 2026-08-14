// Command loadgen drives the payment webhook at a target rate and reports
// latency distribution.
//
// The brief's headline constraint is 100,000 payment notifications per minute.
// Asserting a system handles that is cheap; measuring it is the point. This
// lives in the repo rather than depending on k6 so the whole thing runs with
// nothing but Go and Docker, which the reviewer already needs.
//
// Latency is measured from each request's *scheduled* send time, not from when
// a worker got around to it. Measuring from the actual send hides coordinated
// omission: once the system falls behind, requests queue up in the generator and
// every one of them reports a fast service time while the real client waits.
//
//	go run ./cmd/loadgen -rate=100000 -duration=60s
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	scheduled time.Duration // queue wait + service: what a real caller experiences
	service   time.Duration // time on the wire alone
	status    int
	err       bool
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("loadgen: %v", err)
	}
}

func run() error {
	var (
		target      = flag.String("target", "http://localhost:8080", "base URL of the API")
		secret      = flag.String("secret", "dev-secret-do-not-use-in-production", "HMAC signing secret")
		ratePerMin  = flag.Int("rate", 100_000, "requests per minute")
		duration    = flag.Duration("duration", 60*time.Second, "how long to sustain the rate")
		customers   = flag.Int("customers", 100_000, "size of the seeded customer book")
		concurrency = flag.Int("concurrency", 256, "in-flight requests")
		amountNaira = flag.Int("amount", 10_000, "payment amount in naira")
	)
	flag.Parse()

	if *ratePerMin <= 0 || *duration <= 0 || *customers <= 0 || *concurrency <= 0 {
		return fmt.Errorf("rate, duration, customers and concurrency must all be positive")
	}

	interval := time.Duration(float64(time.Minute) / float64(*ratePerMin))
	planned := int(duration.Seconds() * float64(*ratePerMin) / 60)

	fmt.Fprintf(os.Stderr, "target      %s\n", *target)
	fmt.Fprintf(os.Stderr, "rate        %d req/min (%.0f req/sec, one every %s)\n",
		*ratePerMin, float64(*ratePerMin)/60, interval.Round(time.Microsecond))
	fmt.Fprintf(os.Stderr, "duration    %s (%d requests)\n", *duration, planned)
	fmt.Fprintf(os.Stderr, "book        %d customers\n\n", *customers)

	// A shared transport with a generous idle pool: without it the generator
	// spends its time in TCP handshakes and measures its own connection churn.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}

	type job struct {
		index     int
		scheduled time.Time
	}

	var (
		jobs    = make(chan job, *concurrency*4)
		samples = make([]sample, planned)
		sent    atomic.Int64
		wg      sync.WaitGroup
	)

	runID := time.Now().UnixNano()
	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(runID), seed))

			for j := range jobs {
				customer := fmt.Sprintf("GIG%06d", rng.IntN(*customers)+1)
				reference := fmt.Sprintf("LOAD-%d-%d", runID, j.index)
				body := buildPayload(customer, reference, *amountNaira)

				start := time.Now()
				status, err := postPayment(ctx, client, *target, *secret, body)
				finished := time.Now()

				samples[j.index] = sample{
					scheduled: finished.Sub(j.scheduled),
					service:   finished.Sub(start),
					status:    status,
					err:       err != nil,
				}
				sent.Add(1)
			}
		}(uint64(w) + 1)
	}

	// Open-loop generator: requests are scheduled on a fixed cadence regardless
	// of whether earlier ones have finished, which is how a bank's webhook
	// traffic actually arrives.
	//
	// Pacing is batched rather than one tick per request. At 1,667/sec the
	// interval is 600us, well below the ~1ms timer granularity of a typical
	// host, so a per-request ticker fires late and bursty -- and since latency
	// is measured from the ideal schedule, that jitter would be charged to the
	// server. Ticking every 5ms and releasing the requests due in that window
	// keeps the aggregate rate exact and the measurement honest.
	//
	// The channel send blocks when workers fall behind. That is deliberate: the
	// backlog then shows up in scheduled latency, which is exactly the signal
	// that the system is not keeping up.
	const tickInterval = 5 * time.Millisecond
	perTick := float64(*ratePerMin) / 60 * tickInterval.Seconds()

	begin := time.Now()
	go func() {
		defer close(jobs)
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()

		var (
			issued int
			credit float64
		)
		for issued < planned {
			<-ticker.C
			credit += perTick
			for due := int(credit); due > 0 && issued < planned; due-- {
				jobs <- job{
					index:     issued,
					scheduled: begin.Add(time.Duration(float64(issued) * float64(interval))),
				}
				issued++
				credit--
			}
		}
	}()

	// Closed by the main goroutine once the run finishes; this one only reads it.
	done := make(chan struct{})
	go func() {
		progress := time.NewTicker(5 * time.Second)
		defer progress.Stop()
		for {
			select {
			case <-done:
				return
			case <-progress.C:
				n := sent.Load()
				fmt.Fprintf(os.Stderr, "\r  %d/%d sent (%.0f req/sec)   ",
					n, planned, float64(n)/time.Since(begin).Seconds())
			}
		}
	}()

	wg.Wait()
	elapsed := time.Since(begin)
	close(done)
	fmt.Fprintf(os.Stderr, "\r%50s\r", "")

	report(samples[:sent.Load()], elapsed, *ratePerMin)
	return nil
}

func buildPayload(customer, reference string, naira int) []byte {
	return fmt.Appendf(nil,
		`{"customer_id":%q,"payment_status":"COMPLETE","transaction_amount":"%d","transaction_date":%q,"transaction_reference":%q}`,
		customer, naira, time.Now().Format("2006-01-02 15:04:05"), reference)
}

func postPayment(ctx context.Context, client *http.Client, target, secret string, body []byte) (int, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/payments", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Drain the body FULLY. net/http only returns a connection to the idle pool
	// once the body is read to EOF; a partial read silently closes it instead.
	// A payment response carrying a position is larger than any fixed-size peek,
	// so peeking meant every request opened a new connection, and a sustained
	// run exhausted the host's ephemeral ports -- which looks exactly like the
	// server failing under load, and is not.
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, nil
}

func report(samples []sample, elapsed time.Duration, targetRate int) {
	if len(samples) == 0 {
		fmt.Println("no samples collected")
		return
	}

	statuses := map[int]int{}
	var failures int
	scheduled := make([]time.Duration, 0, len(samples))
	service := make([]time.Duration, 0, len(samples))

	for _, s := range samples {
		if s.err {
			failures++
			continue
		}
		statuses[s.status]++
		scheduled = append(scheduled, s.scheduled)
		service = append(service, s.service)
	}

	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i] < scheduled[j] })
	sort.Slice(service, func(i, j int) bool { return service[i] < service[j] })

	achieved := float64(len(samples)) / elapsed.Seconds()

	fmt.Printf("\nrequests      %d in %s\n", len(samples), elapsed.Round(time.Millisecond))
	fmt.Printf("throughput    %.0f req/sec  (%.0f req/min, target %d)\n", achieved, achieved*60, targetRate)
	fmt.Printf("transport     %d failed\n\n", failures)

	fmt.Println("status codes")
	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	for _, code := range codes {
		fmt.Printf("  %d  %7d  %5.1f%%\n", code, statuses[code], 100*float64(statuses[code])/float64(len(samples)))
	}

	fmt.Printf("\n%-14s %10s %10s\n", "latency", "service", "scheduled")
	for _, p := range []struct {
		label string
		q     float64
	}{{"p50", 0.50}, {"p90", 0.90}, {"p95", 0.95}, {"p99", 0.99}, {"p99.9", 0.999}, {"max", 1}} {
		fmt.Printf("%-14s %10s %10s\n", p.label,
			round(percentile(service, p.q)), round(percentile(scheduled, p.q)))
	}
	fmt.Println("\nservice   = time on the wire")
	fmt.Println("scheduled = queue wait + service, measured from the intended send time")
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

func round(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(10 * time.Microsecond).String()
	}
	return d.Round(100 * time.Microsecond).String()
}
