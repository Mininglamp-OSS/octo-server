package common

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// systemSettingClampTotal counts read-side clamp events emitted by
// warnClampOnce (design §5.2). A clamp means an admin configured a value above
// the server-side hard cap and the effective value was pulled down to the cap —
// i.e. the stored value is not what is being served. A spike is the operator
// signal for "somebody set a knob past its ceiling".
//
// Labelled by the fully qualified setting key (e.g.
// "drive.upload_max_size_kb"), which is a bounded, code-defined set — never a
// free-form value or the configured number.
var systemSettingClampTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "octo",
	Name:      "system_setting_clamp_total",
	Help: "Read-side clamp events: a system_setting int knob whose configured " +
		"value exceeded its server-side hard cap and was clamped for serving. " +
		"Labelled by the fully qualified setting key.",
}, []string{"key"})
