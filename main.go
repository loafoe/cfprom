// Copyright 2018 Andy Lo-A-Foe. All rights reserved.
// Use of this source code is governed by Apache-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"

	"github.com/cloudfoundry-community/go-cfenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	addr     = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
	cpuGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cpu_usage",
			Help: "CPU usage",
		},
		[]string{"org", "space", "app", "instance_index"})
	memGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mem_usage",
			Help: "Memory usage",
		},
		[]string{"org", "space", "app", "instance_index"})
)

func init() {
	prometheus.MustRegister(cpuGauge)
	prometheus.MustRegister(memGauge)
}

type config struct {
	cfclient.Config
	SpaceID string
	AppID   string
}

type bootstrapRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type bootstrapResponse struct {
	Bootstrapped bool   `json:"bootstrapped"`
	Status       string `json:"status"`
}

func main() {
	flag.Parse()

	c := config{
		cfclient.Config{
			ApiAddress: getCFAPI(),
			Username:   os.Getenv("CF_USERNAME"),
			Password:   os.Getenv("CF_PASSWORD"),
		},
		"",
		"",
	}
	appEnv, err := cfenv.Current()
	if err != nil {
		fmt.Printf("Not running in CF. Exiting..\n")
		return
	}
	c.AppID = appEnv.AppID
	c.SpaceID = appEnv.SpaceID

	ch := make(chan config)

	go monitor(ch)

	ch <- c // Initial config

	http.Handle("/metrics", basicAuth(promhttp.Handler()))
	http.Handle("/bootstrap", basicAuth(bootstrapHandler(ch)))
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func (r *bootstrapRequest) valid() bool {
	return r.Username != "" && r.Password != ""
}

func basicAuth(h http.Handler) http.Handler {
	password := os.Getenv("PASSWORD")
	if password == "" { // Noop
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok {
			if u == "cfprom" && p == password {
				h.ServeHTTP(w, r)
				return
			}
		}
		if p, ok := r.URL.Query()["p"]; ok && len(p[0]) > 0 {
			if p[0] == password {
				h.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "access denied", http.StatusUnauthorized)
	})
}

func getCFAPI() string {
	CFAPI := os.Getenv("CF_API")
	if CFAPI != "" {
		return CFAPI
	}
	appEnv, err := cfenv.Current()
	if err != nil {
		return cfclient.DefaultConfig().ApiAddress
	}
	return appEnv.CFAPI

}

func bootstrapHandler(ch chan config) http.Handler {
	var bootstrapped = false

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b bootstrapRequest
		var resp bootstrapResponse

		if req.Method == http.MethodGet {
			resp.Bootstrapped = bootstrapped
			resp.Status = "OK"
			js, err := json.Marshal(resp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(js)
			return
		}
		decoder := json.NewDecoder(req.Body)
		err := decoder.Decode(&b)
		defer req.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Reconfigure
		if b.valid() {
			c := config{
				cfclient.Config{
					ApiAddress: getCFAPI(),
					Username:   b.Username,
					Password:   b.Password,
				},
				"",
				"",
			}
			appEnv, err := cfenv.Current()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			c.AppID = appEnv.AppID
			c.SpaceID = appEnv.SpaceID
			ch <- c // Magic
			bootstrapped = true
			resp.Bootstrapped = bootstrapped
			resp.Status = "OK"
		} else {
			resp.Status = "ERROR: missing username an/or password"
		}
		js, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})
}

func monitor(ch chan config) {
	var loggedIn = false
	var client *cfclient.Client
	var apps []cfclient.App
	var activeConfig config
	var spaceName = ""
	var orgName = ""

	check := time.NewTicker(time.Second * 15)
	refresh := time.NewTicker(time.Second * 15 * 60)

	for {
		select {
		case newConfig := <-ch:
			// Configure
			fmt.Println("Logging in after receiving configuration")
			newClient, err := cfclient.NewClient(&newConfig.Config)
			if err != nil {
				fmt.Printf("Error logging in: %v\n", err)
				continue
			}
			client = newClient
			activeConfig = newConfig
			fmt.Printf("Fetching apps in space: %s\n", activeConfig.SpaceID)
			q := url.Values{}
			q.Add("q", fmt.Sprintf("space_guid:%s", activeConfig.SpaceID))
			apps, _ = client.ListAppsByQuery(q)
			app := apps[0]
			app, _ = client.GetAppByGuid(app.Guid)
			space, _ := app.Space()
			org, _ := space.Org()
			spaceName = space.Name
			orgName = org.Name
			loggedIn = true
		case <-refresh.C:
			if activeConfig.Config.Password == "" {
				fmt.Println("No configuration available during refresh")
				continue
			}
			fmt.Println("Refreshing login")
			newClient, err := cfclient.NewClient(&activeConfig.Config)
			if err != nil {
				fmt.Printf("Error refreshing login: %v\n", err)
				continue
			}
			client = newClient
			q := url.Values{}
			q.Add("q", fmt.Sprintf("space_guid:%s", activeConfig.SpaceID))
			apps, _ = client.ListAppsByQuery(q)
		case <-check.C:
			if !loggedIn {
				continue
			}
			start := time.Now()
			for _, app := range apps {
				if app.Guid == activeConfig.AppID { // Skip self
					continue
				}
				stats, _ := client.GetAppStats(app.Guid)
				for i, s := range stats {
					cpuGauge.WithLabelValues(orgName, spaceName, app.Name, i).Set(s.Stats.Usage.CPU * 100)
					memGauge.WithLabelValues(orgName, spaceName, app.Name, i).Set(float64(s.Stats.Usage.Mem))
				}
			}
			fmt.Printf("Fetching stats of %d apps took %s\n", len(apps), time.Since(start))
		}
	}
}
