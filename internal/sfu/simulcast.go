package sfu

import "time"

const (
	quarterResolution = "q"
	halfResolution    = "h"
	fullResolution    = "f"
)

type SimulcastConfig struct {
	BestQualityFirst bool `json:"bestQualityFirst"`
}

type simulcastTrackHelpers struct {
	switchDelay time.Time
	lastTSCalc  int64
}
