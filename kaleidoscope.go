package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var conf config

// config represents the application configuration
type config struct {
	url           string
	interval      time.Duration
	minCompletion float64
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
	URLs           []*Mirror `json:"urls"`
}

func init() {
	flag.StringVar(&conf.url, "url", "https://www.archlinux.org/mirrors/status/json/", "upstream mirror information URL")
	flag.DurationVar(&conf.interval, "interval", time.Hour, "auto update time in minutes")
	flag.Float64Var(&conf.minCompletion, "completion", 1.0, "minimum mirror completion threshold")
}

func main() {
	// parse the command line flags
	flag.Parse()

	log.Print("Starting...")

	// we want atomic writes to the global mirror status data
	var status atomic.Value

	// run the autoupdater forever
	go update(&status, &conf)

	// handle the endpoints
	mux := http.NewServeMux()

	countryHandler := methodOnly("GET", makeCountryHandler(&status))
	globalHandler := methodOnly("GET", makeGlobalHandler(&status))
	mux.HandleFunc("/country/", countryHandler)
	mux.HandleFunc("/global", globalHandler)

	// serve forever
	http.ListenAndServe(":9000", mux)
}

// update performs a mirror status update whenever the ticker ticks, i.e.
// once per configured interval.
func update(status *atomic.Value, c *config) {
	log.Printf("Auto updater started (interval %s).", c.interval)
	ticker := time.NewTicker(c.interval)
	// perform an update operation once per tick forever
	for {
		log.Print("Performing auto update...")
		newM, err := getData(c)
		// we might recover next tick, so log the error and move on.
		if err != nil {
			log.Print(err)
			continue
		}
		// store the new configuration in a globally atomic operation
		status.Store(newM)
		log.Print("Auto update complete.")
		<-ticker.C
	}
}

// getData downloads and parses the mirror data from the configured URL. It also
// filters mirrors for completion percentage and
func getData(c *config) (*MirrorStatus, error) {
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
	totalCount, httpCount, completeCount := len(m.URLs), 0, 0
	newURLs := make([]*Mirror, 0, totalCount)

	// filter mirrors based on completion and protocol
	for _, url := range m.URLs {
		var isHTTP, isComplete bool

		if url.Protocol == "http" {
			httpCount++
			isHTTP = true
		}

		if url.Completion >= c.minCompletion {
			completeCount++
			isComplete = true
		}

		if isHTTP && isComplete {
			newURLs = append(newURLs, url)
		}
	}

	m.URLs = newURLs

	log.Printf("Mirror stats: Total: %d, HTTP: %d, Complete: %d. HTTP and Complete: %d", totalCount, httpCount, completeCount, len(newURLs))

	return &m, nil
}

// methodOnly restricts HTTP handlers to receiving a single HTTP method only.
func methodOnly(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, http.StatusText(405), 405)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// makeGlobalHandler handles worldwide mirrors, and naïvely returns any valid
// mirror.
func makeGlobalHandler(val *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := val.Load().(*MirrorStatus)

		// an empty mirror list is an error
		if len(status.URLs) == 0 {
			http.Error(w, http.StatusText(500), 500)
		}

		pick := rand.Intn(len(status.URLs))
		http.Redirect(w, r, status.URLs[pick].URL, http.StatusFound)
	}
}

// makeCountryHandler handlers mirrors within a two-letter country code and will
// fail gracefully (i.e. with appropriate HTTP error) when the provided country
// code is invalid or no valid mirrors are available for the country.
func makeCountryHandler(val *atomic.Value) http.HandlerFunc {
	// match against two digit country codes
	re := regexp.MustCompile(`/([A-Za-z]{2})$`)
	return func(w http.ResponseWriter, r *http.Request) {
		matches := re.FindStringSubmatch(r.URL.Path)

		// no match means a bogus country name
		if len(matches) == 0 {
			http.NotFound(w, r)
			return
		}

		// all mirror information contains uppercase country codes
		countryCode := strings.ToUpper(matches[1])

		status := val.Load().(*MirrorStatus)

		// an empty mirror list is an error
		if len(status.URLs) == 0 {
			http.Error(w, http.StatusText(500), 500)
		}

		// filter mirrors based on their country code
		res := make([]*Mirror, 0, len(status.URLs))

		for i, mirror := range status.URLs {
			if mirror.CountryCode == countryCode && mirror.Completion >= 1.0 {
				res = append(res, status.URLs[i])
			}
		}

		// an empty list means no mirrors were found
		if len(res) == 0 {
			http.NotFound(w, r)
			return
		}

		// choose a random mirror from the list and send the user to it
		pick := rand.Intn(len(res))
		http.Redirect(w, r, res[pick].URL, http.StatusFound)
	}
}
