package meter

import (
	"strings"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/evcc-io/evcc/util/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var acceptable = []string{
	api.ErrMissingCredentials.Error(),
	api.ErrMissingToken.Error(),
	"invalid plugin source: ...",
	"missing mqtt broker configuration",
	"mqtt not configured",
	"not a SunSpec device",
	"connect: connection refused", // sockets
	"power: timeout",              // sockets
	"missing password",            // Powerwall
	"connect: no route to host",
	"connect: connection refused",
	"connect: network is unreachable",
	"i/o timeout",
	"timeout",                      // RCT
	"'sma': missing uri or serial", // SMA
	"[1ESY1161052714 1ESY1161229249 1EMH0008842285 1ESY1161978584 1EMH0004864048 1ESY1161979033 7ELS8135823805]", // Discovergy
	"can only have either uri or device",                                   // modbus
	"connection already registered with different protocol: localhost:502", // modbus
	"(Client.Timeout exceeded while awaiting headers)",                     // http
	"context deadline exceeded",                                            // LG ESS
	"no ping response for 192.0.2.2",                                       // SMA
	"no Speedwire ping response for 127.0.0.1",                             // SMA
	"no such network interface",                                            // SMA
	"missing config values: username, password, key",                       // E3DC
	"missing access key",                                                   // Ecoflow
	"eebus not configured",                                                 // EEBus
	"missing token",                                                        // HomeAssistant
}

func TestPeakDischargeTemplate(t *testing.T) {
	for _, name := range []string{"fronius-gen24", "fronius-vertoplus", "sunspec-inverter-control"} {
		t.Run(name, func(t *testing.T) {
			tmpl, err := templates.ByName(templates.Meter, name)
			require.NoError(t, err)

			values := map[string]any{"usage": "battery", "host": "192.0.2.2"}
			b, _, err := tmpl.RenderResult(templates.Meter, templates.RenderModeInstance, values)
			require.NoError(t, err)
			assert.NotContains(t, string(b), "maxdischarge:")

			values["maxdischargepower"] = 5000
			b, _, err = tmpl.RenderResult(templates.Meter, templates.RenderModeInstance, values)
			require.NoError(t, err)
			out := string(b)
			assert.Contains(t, out, "maxdischarge:")
			assert.Contains(t, out, "124:0:StorCtl_Mod")
			assert.Contains(t, out, "124:0:OutWRte")
			assert.True(t, strings.Contains(out, "scale: 0.02"), out)
		})
	}
}

func TestTemplates(t *testing.T) {
	templates.TestClass(t, templates.Meter, func(t *testing.T, values map[string]any) {
		t.Helper()

		if _, err := NewFromConfig(t.Context(), "template", values); err != nil && !test.Acceptable(err, acceptable) {
			t.Log(values)
			t.Error(err)
		}
	})
}
