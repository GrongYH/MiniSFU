package sfu

const (
	quarterResolution = "q"
	halfResolution    = "h"
	fullResolution    = "f"
)

type SimulcastConfig struct {
	BestQualityFirst    bool `json:"bestQualityFirst"`
	EnableTemporalLayer bool `json:"enableTemporalLayer"`
}
