package core

import (
	"errors"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/circuit"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecidePeakShaving(t *testing.T) {
	const limit = 10000.0

	for _, tc := range []struct {
		name          string
		reserve, soc  float64
		grid, battery float64
		maxPower      float64
		readable      bool
		action        peakShavingAction
		discharge     float64
	}{
		{"off", 0, 10, 12000, 0, limit, true, peakShavingNone, 0},
		{"no limit", 30, 10, 12000, 0, 0, true, peakShavingNone, 0},
		{"unreadable", 30, 10, 12000, 0, limit, false, peakShavingSkip, 0},
		{"above reserve", 30, 40, 2000, 0, limit, true, peakShavingNone, 0},
		{"above reserve at limit", 30, 40, limit, 0, limit, true, peakShavingNone, 0},
		{"below reserve under limit", 30, 20, 2000, 0, limit, true, peakShavingHold, 0},
		{"at reserve under limit", 30, 30, 2000, 0, limit, true, peakShavingHold, 0},
		{"below reserve at limit idle", 30, 20, limit, 0, limit, true, peakShavingDischarge, 0},
		{"import above limit", 30, 20, 12000, 0, limit, true, peakShavingDischarge, 2000},
		{"measured discharge keeps the peak", 30, 20, 9000, 4000, limit, true, peakShavingDischarge, 3000},
		{"peak over", 30, 20, 6000, 1000, limit, true, peakShavingHold, 0},
		{"just below band", 30, 20, 9499, 0, limit, true, peakShavingHold, 0},
		{"band unlocks with zero setpoint", 30, 20, 9500, 0, limit, true, peakShavingDischarge, 0},
		{"charging does not hide import", 30, 20, 12000, -3000, limit, true, peakShavingDischarge, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePeakShaving(tc.reserve, tc.soc, tc.grid, tc.battery, tc.maxPower, tc.readable)
			assert.Equal(t, tc.action, got.action)
			assert.InDelta(t, tc.discharge, got.discharge, 1)
		})
	}
}

// peakBattery records battery mode and discharge-ceiling commands.
type peakBattery struct {
	api.Meter
	modes        []api.BatteryMode
	modeCalls    []api.BatteryMode
	discharge    []float64
	dischargeErr error
}

func (b *peakBattery) BatteryModes() []api.BatteryMode {
	if b.modes == nil {
		return []api.BatteryMode{api.BatteryNormal, api.BatteryHold, api.BatteryCharge, api.BatteryDischarge}
	}
	return b.modes
}

func (b *peakBattery) SetBatteryMode(mode api.BatteryMode) error {
	b.modeCalls = append(b.modeCalls, mode)
	return nil
}

func (b *peakBattery) SetMaxDischarge(power float64) error {
	if b.dischargeErr != nil {
		return b.dischargeErr
	}
	b.discharge = append(b.discharge, power)
	return nil
}

// peakMinSoc adds the device's own soc bounds.
type peakMinSoc struct{ *peakBattery }

func (peakMinSoc) GetSocLimits() (float64, float64, error) { return 10, 100, nil }

// modeOnlyBattery can hold, and cannot take a discharge ceiling.
type modeOnlyBattery struct {
	api.Meter
	calls []api.BatteryMode
}

func (b *modeOnlyBattery) BatteryModes() []api.BatteryMode {
	return []api.BatteryMode{api.BatteryNormal, api.BatteryHold, api.BatteryCharge}
}

func (b *modeOnlyBattery) SetBatteryMode(mode api.BatteryMode) error {
	b.calls = append(b.calls, mode)
	return nil
}

type dimmedHEMS struct{ consumption float64 }

func (*dimmedHEMS) SetUpdated(func())            {}
func (*dimmedHEMS) CurtailedPercent() *int       { return nil }
func (*dimmedHEMS) MaxProductionPower() *float64 { return nil }
func (h *dimmedHEMS) MaxConsumptionPower() *float64 {
	return &h.consumption
}

func newPeakSite(t *testing.T, meter api.Meter, maxPower float64) *Site {
	t.Helper()
	circ, err := circuit.New(util.NewLogger("main"), "main", 0, maxPower, nil, 0)
	require.NoError(t, err)
	return &Site{
		log:            util.NewLogger("foo"),
		batteryMeters:  []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "home"}, meter)},
		circuit:        circ,
		gridPowerOK:    true,
		circuitPowerOK: true,
	}
}

func (site *Site) setPeakReadings(reserve, soc, grid, battery float64) {
	site.peakShavingSoc = reserve
	site.battery.Soc = soc
	site.gridPower = grid
	site.battery.Power = battery
}

func fastChargingLoadpoint() *Loadpoint {
	lp := NewLoadpoint(util.NewLogger("lp"), nil)
	lp.status = api.StatusC
	lp.mode = api.ModeNow
	return lp
}

func TestPeakShavingReserve(t *testing.T) {
	const limit = 10000.0

	t.Run("off", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(0, 10, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("above reserve", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 40, 2000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("below reserve", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 2000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryHold}, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("at limit idle", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, limit, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		require.Len(t, bat.discharge, 1)
		assert.InDelta(t, 0, bat.discharge[0], 1)
		assert.Empty(t, bat.modeCalls)
	})

	t.Run("import above limit", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		require.Len(t, bat.discharge, 1)
		assert.InDelta(t, 2000, bat.discharge[0], 1)
		assert.Empty(t, bat.modeCalls)
		assert.NotEqual(t, api.BatteryNormal, site.GetBatteryMode())
	})

	t.Run("already discharging", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 9000, 4000)
		site.updateBatteryMode(false, false, api.Rate{})
		require.Len(t, bat.discharge, 1)
		assert.InDelta(t, 3000, bat.discharge[0], 1)
		assert.NotContains(t, bat.modeCalls, api.BatteryHold)
		assert.NotContains(t, bat.modeCalls, api.BatteryNormal)
	})

	t.Run("peak over", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 6000, 1000)
		site.peakDischarge = map[string]float64{"home": 1000}
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryHold}, bat.modeCalls)
		assert.Empty(t, bat.discharge)
		assert.Empty(t, site.peakDischarge)
	})

	t.Run("above reserve at limit", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 50, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("fast charge at limit", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.batteryDischargeControl = true
		site.loadpoints = []*Loadpoint{fastChargingLoadpoint()}
		site.updateBatteryMode(false, false, api.Rate{})
		require.Len(t, bat.discharge, 1)
		assert.InDelta(t, 2000, bat.discharge[0], 1)
		assert.NotContains(t, bat.modeCalls, api.BatteryHold)
	})

	t.Run("external mode", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.batteryModeExternal = api.BatteryCharge
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryCharge}, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("grid charge pauses and resumes", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(true, false, api.Rate{})
		require.Len(t, bat.discharge, 1)
		assert.InDelta(t, 2000, bat.discharge[0], 1)
		assert.Empty(t, bat.modeCalls)

		site.gridPower = 2000
		site.battery.Power = 0
		site.updateBatteryMode(true, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryCharge}, bat.modeCalls)
		assert.Empty(t, site.peakDischarge)
	})

	t.Run("grid charge under limit", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 2000, 0)
		site.batteryMode = api.BatteryCharge
		site.updateBatteryMode(true, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)
		assert.Equal(t, api.BatteryCharge, site.GetBatteryMode())
	})

	t.Run("hems dim", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.hems = &dimmedHEMS{consumption: 5000}
		site.updateBatteryMode(true, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryHold}, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("measurements missing", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 2000, 0)
		site.gridPowerOK = false
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)

		site.peakShavingEngaged = true
		site.gridPower = 12000
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Empty(t, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("device minimum soc", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, peakMinSoc{bat}, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		soc, min, max := 10.0, 20.0, 100.0
		site.battery.Devices = []types.Measurement{{Soc: &soc, MinSoc: &min, MaxSoc: &max}}
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryHold}, bat.modeCalls)
		assert.Empty(t, bat.discharge)
	})

	t.Run("no discharge ceiling", func(t *testing.T) {
		bat := &modeOnlyBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryHold}, bat.calls)
		assert.NotContains(t, bat.calls, api.BatteryNormal)
	})

	t.Run("release above reserve", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		require.Len(t, bat.discharge, 1)

		site.battery.Soc = 40
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Equal(t, []api.BatteryMode{api.BatteryNormal}, bat.modeCalls)
		assert.Empty(t, site.peakDischarge)
		assert.Len(t, bat.discharge, 1)
	})

	t.Run("apply error", func(t *testing.T) {
		bat := &peakBattery{dischargeErr: errors.New("write failed")}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		assert.False(t, site.peakShavingEngaged)
		assert.Empty(t, bat.modeCalls)
	})

	t.Run("deadband", func(t *testing.T) {
		bat := &peakBattery{}
		site := newPeakSite(t, bat, limit)
		site.setPeakReadings(30, 20, 12000, 0)
		site.updateBatteryMode(false, false, api.Rate{})
		site.updateBatteryMode(false, false, api.Rate{})
		assert.Len(t, bat.discharge, 1)
		assert.Empty(t, bat.modeCalls)
	})
}
