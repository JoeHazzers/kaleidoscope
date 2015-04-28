package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var conf config

type selector func(*MirrorStatus, *http.Request) (*Mirror, error)

// config represents the application configuration
type config struct {
	url           string
	interval      time.Duration
	minCompletion float64
	host          string
	port          int
}

// Mirror is a description of an Arch Linux Mirror
type Mirror struct {
	Protocol    string     `json:"protocol"`
	URL         string     `json:"url"`
	Country     string     `json:"country"`
	LastSync    *time.Time `json:"last_sync"`
	Delay       int        `json:"delay"`
	Score       float64    `json:"score"`
	Completion  float64    `json:"completion_pct"`
	CountryCode string     `json:"country_code"`
	DurStdDev   float64    `json:"duration_stddev"`
	DurAvg      float64    `json:"duration_avg"`
}

// MirrorStatus is global status information for mirror checks
type MirrorStatus struct {
	Cutoff         int       `json:"cutoff"`
	CheckFrequency int       `json:"check_frequency"`
	NumChecks      int       `json:"num_checks"`
	LastCheck      time.Time `json:"last_check"`
	Version        int       `json:"version"`
	Global         []*Mirror `json:"urls"`
	Countries      map[string][]*Mirror
}

// ByScore implements sort.Interface for []*Mirror based on the Score field.
type ByScore []*Mirror

func (s ByScore) Len() int           { return len(s) }
func (s ByScore) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s ByScore) Less(i, j int) bool { return s[i].Score < s[j].Score }

func init() {
	flag.StringVar(&conf.url, "url", "https://www.archlinux.org/mirrors/status/json/", "upstream mirror information URL")
	flag.DurationVar(&conf.interval, "interval", time.Hour, "auto update time in minutes")
	flag.Float64Var(&conf.minCompletion, "completion", 1.0, "minimum mirror completion threshold")
	flag.StringVar(&conf.host, "host", "0.0.0.0", "host to listen for connections on")
	flag.IntVar(&conf.port, "port", 9090, "port to listen for on")
}

func main() {
	// parse the command line flags
	flag.Parse()

	log.Print("Starting...")

	// we want atomic writes to the global mirror status data
	var status atomic.Value
	done := make(chan bool)

	// run the autoupdater forever
	go update(&status, &conf, done)

	// handle the endpoints
	mux := http.NewServeMux()

	countryHandler := http.StripPrefix("/country", redirector(&status, countrySelector()))
	globalHandler := http.StripPrefix("/global", redirector(&status, globalSelector))
	mux.HandleFunc("/country/", countryHandler.(http.HandlerFunc))
	mux.HandleFunc("/global/", globalHandler.(http.HandlerFunc))

	addr := fmt.Sprintf("%s:%d", conf.host, conf.port)

	// serve forever
	<-done
	log.Printf("Init finished. Listening on %s", addr)
	http.ListenAndServe(addr, mux)
}

// update performs a mirror status update whenever the ticker ticks, i.e.
// once per configured interval.
func update(status *atomic.Value, c *config, done chan<- bool) {
	log.Printf("Auto updater started (interval %s).", c.interval)
	ticker := time.NewTicker(c.interval)
	var once sync.Once
	// perform an update operation once per tick forever
	for {
		log.Print("Performing auto update...")
		newM, err := getMirrorInfo(c)
		// we might recover next tick, so log the error and move on.
		if err != nil {
			log.Print(err)
			continue
		}
		// store the new configuration in a globally atomic operation
		status.Store(newM)
		log.Print("Auto update complete.")
		once.Do(func() {
			done <- true
			close(done)
		})
		<-ticker.C
	}
}

// getMirrorInfo downloads and parses the mirror data from the configured URL.
// It also filters mirrors for completion percentage and HTTP protocol.
func getMirrorInfo(c *config) (*MirrorStatus, error) {
	log.Printf("Downloading mirror list from '%s'...", c.url)
	resp, err := http.Get(c.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var m MirrorStatus

	// unmarshal the retrieved JSON data
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&m)
	if err != nil {
		return nil, err
	}

	// nice reporting statistics

	log.Printf("Filtering mirrors with HTTP and completion>=%f...", c.minCompletion)
	totalCount, httpCount, completeCount := len(m.Global), 0, 0
	newMirrors := make([]*Mirror, 0, totalCount)
	m.Countries = make(map[string][]*Mirror)

	sort.Stable(sort.Reverse(ByScore(m.Global)))

	// filter mirrors based on completion and protocol
	for _, mirror := range m.Global {
		var isHTTP, isComplete bool

		if mirror.Protocol == "http" {
			httpCount++
			isHTTP = true
		}

		if mirror.Completion >= c.minCompletion {
			completeCount++
			isComplete = true
		}

		if isHTTP && isComplete {
			newMirrors = append(newMirrors, mirror)
			country, ok := m.Countries[mirror.CountryCode]
			if !ok {
				country = make([]*Mirror, 0)
			}
			m.Countries[mirror.CountryCode] = append(country, mirror)
		}
	}

	m.Global = newMirrors

	log.Printf("Mirror stats: Total: %d, HTTP: %d, Complete: %d. HTTP and Complete: %d", totalCount, httpCount, completeCount, len(m.Global))

	return &m, nil
}

func redirector(status *atomic.Value, s selector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, http.StatusText(405), 405)
			return
		}

		c := status.Load().(*MirrorStatus)
		if len(c.Global) == 0 {
			http.Error(w, http.StatusText(500), 500)
			return
		}
		mirror, err := s(c, r)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}

		url, err := url.Parse(mirror.URL)
		if err != nil {
			http.Error(w, http.StatusText(500), 500)
			return
		}

		url.Path = path.Join(url.Path, r.URL.Path)

		http.Redirect(w, r, url.String(), 302)
	}
}

func globalSelector(status *MirrorStatus, r *http.Request) (*Mirror, error) {
	return status.Global[0], nil
}

func countrySelector() selector {
	re := regexp.MustCompile(`^/([a-z]{2})(?:/|$)`)
	return func(status *MirrorStatus, r *http.Request) (*Mirror, error) {
		res := re.FindStringSubmatch(r.URL.Path)
		if len(res) != 2 {
			return nil, fmt.Errorf("invalid country code %+v", res)
		}

		country, ok := status.Countries[strings.ToUpper(res[1])]
		if !ok {
			return nil, errors.New("country not found")
		}

		r.URL.Path = r.URL.Path[len(res[0]):]
		return country[0], nil
	}
}
