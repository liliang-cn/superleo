// Package watchdog is the part of SuperLeo that lives outside it: a timer in
// its own process that asks the running app, through its local API only,
// whether it is alive, whether its runs make progress, whether they are on
// track and whether its schedules fired.
package watchdog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is ~/.superleo/watchdog.json. Every field is optional.
type Config struct {
	TickSeconds         int    `json:"tick_seconds"`
	APIURL              string `json:"api_url"`
	AppPath             string `json:"app_path"`
	APITimeoutSeconds   int    `json:"api_timeout_seconds"`
	HangEscalateMinutes int    `json:"hang_escalate_minutes"`
	RelaunchMax         int    `json:"relaunch_max"`
	RelaunchWindowMin   int    `json:"relaunch_window_minutes"`
	RelaunchGapSeconds  int    `json:"relaunch_gap_seconds"`
	StallAfterMinutes   int    `json:"stall_after_minutes"`
	DriftAfterMinutes   int    `json:"drift_after_minutes"`
	DriftEveryMinutes   int    `json:"drift_every_minutes"`
	ScheduleGraceMin    int    `json:"schedule_grace_minutes"`
	LogMaxMB            int    `json:"log_max_mb"`
	// MacNotify turns the osascript notification on (default true).
	MacNotify *bool `json:"mac_notify"`
	// Judge overrides the model asked about drift; empty fields take the
	// SuperLeo settings (llm_base_url, llm_model, llm_key).
	JudgeBaseURL string `json:"judge_base_url"`
	JudgeModel   string `json:"judge_model"`
	JudgeKey     string `json:"judge_key"`
	// Home is the SuperLeo data directory (default ~/.superleo, or SUPERLEO_HOME).
	Home string `json:"-"`
}

func (c Config) Tick() time.Duration       { return time.Duration(c.TickSeconds) * time.Second }
func (c Config) APITimeout() time.Duration { return time.Duration(c.APITimeoutSeconds) * time.Second }
func (c Config) HangAfter() time.Duration  { return time.Duration(c.HangEscalateMinutes) * time.Minute }
func (c Config) RelaunchWindow() time.Duration {
	return time.Duration(c.RelaunchWindowMin) * time.Minute
}
func (c Config) RelaunchGap() time.Duration { return time.Duration(c.RelaunchGapSeconds) * time.Second }
func (c Config) StallAfter() time.Duration  { return time.Duration(c.StallAfterMinutes) * time.Minute }
func (c Config) DriftAfter() time.Duration  { return time.Duration(c.DriftAfterMinutes) * time.Minute }
func (c Config) DriftEvery() time.Duration  { return time.Duration(c.DriftEveryMinutes) * time.Minute }
func (c Config) ScheduleGrace() time.Duration {
	return time.Duration(c.ScheduleGraceMin) * time.Minute
}
func (c Config) macNotify() bool { return c.MacNotify == nil || *c.MacNotify }

// DataDir is where SuperLeo keeps its files.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("SUPERLEO_HOME")); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ".superleo"
	}
	return filepath.Join(h, ".superleo")
}

func (c *Config) defaults() {
	set := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	set(&c.TickSeconds, 60)
	set(&c.APITimeoutSeconds, 5)
	set(&c.HangEscalateMinutes, 10)
	set(&c.RelaunchMax, 3)
	set(&c.RelaunchWindowMin, 30)
	set(&c.RelaunchGapSeconds, 120)
	set(&c.StallAfterMinutes, 20)
	set(&c.DriftAfterMinutes, 15)
	set(&c.DriftEveryMinutes, 10)
	set(&c.ScheduleGraceMin, 5)
	set(&c.LogMaxMB, 4)
	if c.APIURL == "" {
		c.APIURL = "http://127.0.0.1:43119"
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.AppPath == "" {
		c.AppPath = "/Applications/SuperLeo.app"
	}
}

// LoadConfig reads <home>/watchdog.json (a missing file is the defaults).
func LoadConfig(home string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(home, "watchdog.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, errors.New("watchdog.json: " + err.Error())
		}
	}
	c.Home = home
	c.defaults()
	return c, nil
}
