package core

import (
	"errors"
	"math"
	"slices"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/util/config"
)

// peakShavingBand unlocks the reserve at 95% of the circuit limit and locks it
// again only after corrected power has fallen below that same band.
const peakShavingBand = 0.05

// peakShavingDeadband skips rewriting a discharge ceiling that barely changed.
const peakShavingDeadband = 50.0

type peakShavingAction int

const (
	peakShavingNone peakShavingAction = iota
	peakShavingSkip
	peakShavingHold
	peakShavingDischarge
)

type peakShavingDecision struct {
	action    peakShavingAction
	discharge float64
}

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

func (site *Site) batteryConfigured() bool {
	return len(site.batteryMeters) > 0
}

func (site *Site) hasBatteryControl() bool {
	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		if api.HasCap[api.BatteryController](meter) {
			return true
		}
	}

	return false
}

// setBatteryMode sets the battery mode
func (site *Site) setBatteryMode(batMode api.BatteryMode) {
	site.batteryMode = batMode
	site.publish(keys.BatteryMode, batMode)
}

// SetBatteryMode sets the battery mode
func (site *Site) SetBatteryMode(batMode api.BatteryMode) {
	site.Lock()
	defer site.Unlock()

	site.log.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

// fromTo reports whether the battery is entering, holding or leaving mode m:
// either m is requested, or nothing is requested and the battery is already in m.
func (site *Site) fromTo(requested, m api.BatteryMode) bool {
	return requested == m || requested == api.BatteryUnknown && site.batteryMode == m
}

func (site *Site) updateBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) {
	peak := site.peakShavingDecision()
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive, rate)

	// put battery into hold mode when charging is active and HEMS dimmed
	hemsAdjusted := false
	if dimmed := hems.Dimmed(site.hems); site.fromTo(batteryMode, api.BatteryCharge) && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
		hemsAdjusted = true
	}

	// stop discharging to grid when HEMS curtailed production, but keep self-consumption
	if curtailed := hems.Curtailed(site.hems); site.fromTo(batteryMode, api.BatteryDischarge) && curtailed != nil && *curtailed {
		site.log.DEBUG.Println("battery mode: HEMS curtailed")
		batteryMode = api.BatteryNormal
		hemsAdjusted = true
	}

	// external control and a grid-operator limit outrank the reserve
	if !hemsAdjusted && !site.batteryModeExternalPending() {
		switch peak.action {
		case peakShavingSkip:
			if site.peakShavingEngaged {
				site.log.DEBUG.Println("peak shaving: grid or circuit power unavailable")
				return
			}
		case peakShavingDischarge:
			wrote, err := site.applyPeakShavingDischarge(peak.discharge)
			if err != nil {
				site.log.ERROR.Println("battery mode:", err)
				return
			}
			site.peakShavingEngaged = true
			if !wrote {
				site.SetBatteryMode(api.BatteryHold)
			}
			return
		case peakShavingHold:
			// grid charging stays. Anything else locks discharge while the reserve is kept.
			if !batteryGridChargeActive && batteryMode != api.BatteryCharge && batteryMode != api.BatteryHold {
				batteryMode = api.BatteryHold
			}
			// already locked, and no discharge ceiling left to clear
			if batteryMode == api.BatteryHold && site.GetBatteryMode() == api.BatteryHold && len(site.peakDischarge) == 0 {
				batteryMode = api.BatteryUnknown
			}
			site.peakShavingEngaged = true
		case peakShavingNone:
			site.peakShavingEngaged = false
		}
	}

	// a discharge ceiling owns the mode registers, so the following mode must be written again
	batteryMode = site.releasePeakDischarge(batteryMode)

	// NOTE: applyBatteryMode is always called when charge or discharge mode is active to
	// validate max soc / min soc reserve
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge || site.batteryMode == api.BatteryDischarge {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}
}

// batteryModeExternalPending reports an external mode that is active or still winding down.
func (site *Site) batteryModeExternalPending() bool {
	if site.GetBatteryModeExternal() != api.BatteryUnknown {
		return true
	}
	site.RLock()
	defer site.RUnlock()
	return !site.batteryModeExternalTimer.IsZero()
}

// peakShavingDecision reads the reserve, the grid meter and the root circuit limit.
func (site *Site) peakShavingDecision() peakShavingDecision {
	site.RLock()
	reserve := site.peakShavingSoc
	soc := site.battery.Soc
	grid := site.gridPower
	battery := site.battery.Power
	readable := site.gridPowerOK && site.circuitPowerOK
	circuit := site.circuit
	site.RUnlock()

	var maxPower float64
	if circuit != nil {
		maxPower = circuit.GetMaxPower()
	}

	return decidePeakShaving(reserve, soc, grid, battery, maxPower, readable)
}

// releasePeakDischarge forces a mode write after a discharge ceiling.
// Unknown means "already in the published mode"; that mode is written again so the ceiling is cleared.
func (site *Site) releasePeakDischarge(mode api.BatteryMode) api.BatteryMode {
	if len(site.peakDischarge) == 0 || mode != api.BatteryUnknown {
		return mode
	}
	mode = site.GetBatteryMode()
	if mode == api.BatteryUnknown {
		return api.BatteryNormal
	}
	return mode
}

// decidePeakShaving chooses whether the reserve stays locked.
// Positive battery power is discharge. Corrected power is grid import plus that
// discharge, so commanding it does not look like the peak has ended.
// The measured discharge is used once: adding the setpoint on top would raise
// the ceiling every cycle until the inverter caught up.
func decidePeakShaving(reserve, soc, gridImport, batteryPower, maxPower float64, readable bool) peakShavingDecision {
	if reserve <= 0 || maxPower <= 0 {
		return peakShavingDecision{action: peakShavingNone}
	}
	if !readable || math.IsNaN(gridImport) || math.IsNaN(batteryPower) || math.IsInf(gridImport, 0) || math.IsInf(batteryPower, 0) {
		return peakShavingDecision{action: peakShavingSkip}
	}

	corrected := gridImport + math.Max(0, batteryPower)
	active := corrected >= maxPower*(1-peakShavingBand)

	// above the reserve the battery is not kept back, so a peak only throttles controllable loads
	if soc > reserve {
		return peakShavingDecision{action: peakShavingNone}
	}
	if active {
		return peakShavingDecision{
			action:    peakShavingDischarge,
			discharge: math.Max(0, corrected-maxPower),
		}
	}
	return peakShavingDecision{action: peakShavingHold}
}

// applyPeakShavingDischarge commands a discharge ceiling of power watts.
// Devices that cannot limit discharge, or that have reached their own minimum
// soc, are held. Normal mode is not used: it would hand the battery back to
// full self-consumption. wrote reports whether any ceiling was sent.
func (site *Site) applyPeakShavingDischarge(power float64) (bool, error) {
	if power < 0 || math.IsNaN(power) {
		power = 0
	}

	devices := site.state().battery.Devices
	type target struct {
		dev   config.Device[api.Meter]
		index int
		set   func(float64) error
	}
	var targets []target

	for i, dev := range site.batteryMeters {
		meter := dev.Instance()
		if meter == nil {
			continue
		}
		setter, ok := api.Cap[api.BatteryMaxDischarger](meter)
		if !ok {
			continue
		}

		var m types.Measurement
		if i < len(devices) {
			m = devices[i]
		}
		hold, err := site.batterySocLimitReached(dev, m, true)
		if err != nil && !errors.Is(err, api.ErrNotAvailable) {
			return false, err
		}
		if hold {
			continue
		}
		targets = append(targets, target{dev, i, setter.SetMaxDischarge})
	}

	if len(targets) == 0 {
		site.log.DEBUG.Println("peak shaving: no discharge limit, locking battery")
		return false, site.applyBatteryMode(api.BatteryHold)
	}

	share := power / float64(len(targets))

	applied := make(map[int]bool, len(targets))
	wrote := false
	for _, t := range targets {
		limit := site.clampDischarge(t.dev.Instance(), share)
		name := t.dev.Config().Name
		if site.peakDischarge != nil {
			if prev, ok := site.peakDischarge[name]; ok && math.Abs(prev-limit) < peakShavingDeadband {
				applied[t.index] = true
				wrote = true
				continue
			}
		}

		if err := t.set(limit); err != nil {
			if !errors.Is(err, api.ErrNotAvailable) {
				return wrote, err
			}
			continue
		}

		if site.peakDischarge == nil {
			site.peakDischarge = make(map[string]float64)
		}
		site.peakDischarge[name] = limit
		// the ceiling replaced the mode registers, so the next mode must be written again
		delete(site.batteryModeApplied, name)
		applied[t.index] = true
		wrote = true
		site.log.DEBUG.Printf("peak shaving: battery %s discharge up to %.0fW", deviceTitleOrName(t.dev), limit)
	}

	// batteries that cannot take a ceiling, or that are at their minimum soc, stay locked
	for i, dev := range site.batteryMeters {
		if dev.Instance() == nil || applied[i] {
			continue
		}
		if _, ok := api.Cap[api.BatteryController](dev.Instance()); !ok {
			continue
		}
		if err := site.writeBatteryMode(dev, i, api.BatteryHold); err != nil {
			return wrote, err
		}
	}

	return wrote, nil
}

func (site *Site) clampDischarge(meter api.Meter, power float64) float64 {
	lim, ok := api.Cap[api.BatteryPowerLimiter](meter)
	if !ok {
		return power
	}
	_, max, err := lim.GetPowerLimits()
	if err != nil || max <= 0 {
		return power
	}
	return math.Min(power, max)
}

// requiredBatteryMode determines required battery mode based on grid charge/discharge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive, batteryGridDischargeActive bool, rate api.Rate) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()
	extMode := site.GetBatteryModeExternal()

	var extModeReset bool
	if extMode == api.BatteryUnknown {
		site.Lock()
		extModeReset = !site.batteryModeExternalTimer.IsZero()
		site.Unlock()
	}

	keepUnlessModified := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	switch {
	case !site.batteryConfigured():
		res = api.BatteryUnknown
	case extModeReset:
		// require normal mode to leave external control
		res = api.BatteryNormal
	case extMode != api.BatteryUnknown:
		// require external mode only once
		if extMode != batMode {
			res = extMode
		}
	case site.Automatic() && site.unmodelledCharging(rate):
		// the suggestion ignores loads the optimizer cannot model as storage
		res = keepUnlessModified(api.BatteryHold)
	case site.Automatic():
		// optimizer decides, replacing grid charge limit and discharge control
		if mode, ok := site.batterySuggestionMode(); ok {
			res = keepUnlessModified(mode)
		} else if batteryModeModified(batMode) {
			// no suggestion: release the battery
			res = api.BatteryNormal
		}
	case batteryGridChargeActive:
		// independent limits (buy vs feed-in rate) can both be active at once;
		// charge wins to avoid buying and immediately selling
		if batteryGridDischargeActive && batMode != api.BatteryCharge {
			site.log.WARN.Println("battery mode: grid charge and grid discharge both active, charge takes priority")
		}
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate) || (batteryGridDischargeActive && site.evFastChargingActive()):
		// hold wins over feed-in discharge; fast charging holds even without
		// batteryDischargeControl, selling while an EV fast-charges is worse
		res = keepUnlessModified(api.BatteryHold)
	case batteryGridDischargeActive:
		res = keepUnlessModified(api.BatteryDischarge)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// unmodelledCharging reports a loadpoint charging at full power that the optimizer
// does not control: it cannot model it as storage (unknown vehicle capacity, see
// optimizerRequest), it is a heater, or only the battery is automatic. Its battery
// suggestion does not account for that load, so the battery must be held. Like
// dischargeControlActive this includes charging on the smart cost limit, which is
// what keeps such a loadpoint running at cheap prices.
func (site *Site) unmodelledCharging(rate api.Rate) bool {
	for _, lp := range site.activeLoadpoints() {
		if lp.optimizerControlled() {
			continue
		}

		if lp.GetStatus() == api.StatusC && (site.smartCostActive(lp, rate) || lp.IsFastChargingActive()) {
			return true
		}
	}

	return false
}

// batterySuggestionMode returns the optimizer's mode for the first controllable battery.
// TODO apply per battery once the site tracks more than a single battery mode
func (site *Site) batterySuggestionMode() (api.BatteryMode, bool) {
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}

		s := site.suggestion(batteryKey(dev.Config().Name), site.GetBatteryMode().String())
		if s == nil {
			continue
		}

		mode, err := api.BatteryModeString(s.Action)
		if err != nil {
			// unknown action, release the battery
			site.log.DEBUG.Printf("battery %s: cannot apply suggestion %s", deviceTitleOrName(dev), s.Action)
			return api.BatteryNormal, true
		}

		return mode, true
	}

	return api.BatteryUnknown, false
}

// batterySocLimitReached reports whether the battery has reached the soc bound
// that should stop the requested mode: the max soc when charging, or the min
// soc reserve when discharging to grid. A configured limit of 0 disables the
// respective check (max is also disabled at 100). Soc and limits come from the
// cycle's measurement so the device is not read again.
func (site *Site) batterySocLimitReached(dev config.Device[api.Meter], m types.Measurement, discharge bool) (bool, error) {
	if _, ok := api.Cap[api.BatterySocLimiter](dev.Instance()); !ok {
		return false, nil
	}

	if m.Soc == nil || m.MinSoc == nil || m.MaxSoc == nil {
		return false, errors.New("battery soc or soc limits not available")
	}

	soc, minSoc, maxSoc := *m.Soc, *m.MinSoc, *m.MaxSoc

	if discharge {
		if minSoc > 0 && soc <= minSoc {
			site.log.DEBUG.Printf("battery %s: reserve soc reached (%.0f <= %.0f)", deviceTitleOrName(dev), soc, minSoc)
			return true, nil
		}
		return false, nil
	}

	if maxSoc > 0 && maxSoc < 100 && soc >= maxSoc {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f >= %.0f)", deviceTitleOrName(dev), soc, maxSoc)
		return true, nil
	}

	return false, nil
}

// batteryModeFallbacks lists the modes applied instead of an unsupported mode, most to least aligned with its intent
var batteryModeFallbacks = map[api.BatteryMode][]api.BatteryMode{
	api.BatteryCharge:     {api.BatteryHold, api.BatteryNormal},       // keep the energy while grid power is cheap
	api.BatteryDischarge:  {api.BatteryHoldCharge, api.BatteryNormal}, // don't store pv, feed the house instead
	api.BatteryHold:       {api.BatteryNormal},
	api.BatteryHoldCharge: {api.BatteryNormal},
}

// supportedBatteryMode returns the requested mode if supported, otherwise the first supported fallback or unknown
func supportedBatteryMode(supported []api.BatteryMode, mode api.BatteryMode) api.BatteryMode {
	for _, m := range append([]api.BatteryMode{mode}, batteryModeFallbacks[mode]...) {
		if slices.Contains(supported, m) {
			return m
		}
	}
	return api.BatteryUnknown
}

// applyBatteryMode applies the mode to each battery.
//
// A battery that reached the soc bound of the requested mode is held instead:
// the max soc when charging, the min soc reserve when discharging to grid. This
// is decided per device, so one battery reaching its bound does not force the
// others into hold.
func (site *Site) applyBatteryMode(mode api.BatteryMode) error {
	fromToCharge := site.fromTo(mode, api.BatteryCharge)
	fromToDischarge := site.fromTo(mode, api.BatteryDischarge)

	devices := site.state().battery.Devices

	for i, dev := range site.batteryMeters {
		if dev.Instance() == nil {
			continue
		}
		if _, ok := api.Cap[api.BatteryController](dev.Instance()); !ok {
			continue
		}

		// per-device mode so one battery reaching its soc bound does not affect the others
		deviceMode := mode

		// hold at the soc bound of the requested mode (max soc for charge, min soc reserve for grid discharge)
		if (fromToCharge || fromToDischarge) && deviceMode != api.BatteryHold {
			var m types.Measurement
			if i < len(devices) {
				m = devices[i]
			}

			hold, err := site.batterySocLimitReached(dev, m, fromToDischarge)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
			if hold {
				deviceMode = api.BatteryHold
			}
		}

		if err := site.writeBatteryMode(dev, i, deviceMode); err != nil {
			return err
		}
	}

	return nil
}

// writeBatteryMode applies mode to one battery. A peak-shaving discharge ceiling
// is cleared first so the mode's registers are written again.
func (site *Site) writeBatteryMode(dev config.Device[api.Meter], _ int, mode api.BatteryMode) error {
	if mode == api.BatteryUnknown {
		return nil
	}

	meter := dev.Instance()
	batCtrl, ok := api.Cap[api.BatteryController](meter)
	if !ok {
		return nil
	}

	// an unsupported mode falls back to the closest supported one instead of leaving the battery in the mode applied before
	applyMode := supportedBatteryMode(batCtrl.BatteryModes(), mode)
	if applyMode == api.BatteryUnknown {
		return nil
	}

	if site.batteryModeApplied == nil {
		site.batteryModeApplied = make(map[string]api.BatteryMode)
	}

	name := dev.Config().Name
	// the discharge ceiling changed the same registers the mode owns
	force := false
	if _, ok := site.peakDischarge[name]; ok {
		force = true
		delete(site.peakDischarge, name)
	}
	if !force && applyMode == site.batteryModeApplied[name] {
		return nil
	}

	if applyMode != mode {
		site.log.WARN.Printf("battery %s does not support mode %s, using %s", deviceTitleOrName(dev), mode, applyMode)
	}

	if err := batCtrl.SetBatteryMode(applyMode); err != nil {
		if !errors.Is(err, api.ErrNotAvailable) {
			return err
		}
		return nil
	}

	site.batteryModeApplied[name] = applyMode
	site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), applyMode)
	return nil
}

func (site *Site) tariffRates(usage api.TariffUsage) (api.Rates, error) {
	tariff := site.GetTariff(usage)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

// smartCostActive reports whether the loadpoint charges on its smart cost limit.
// The limit only acts in smart mode (see Loadpoint.Update), other modes ignore it.
func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	if lp.GetMode() != api.ModeSmart {
		return false
	}
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

// batteryGridDischargeActive reports whether the feed-in rate has reached the
// grid discharge limit; the opt-in gates both this and the optimizer's planning
func (site *Site) batteryGridDischargeActive(rate api.Rate) bool {
	if !site.GetBatteryGridDischarge() {
		return false
	}

	limit := site.GetBatteryGridDischargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value >= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.activeLoadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && (smartCostActive || lp.IsFastChargingActive()) {
			return true
		}
	}

	return false
}

// evFastChargingActive reports whether any loadpoint is fast charging,
// regardless of the batteryDischargeControl opt-in
func (site *Site) evFastChargingActive() bool {
	for _, lp := range site.activeLoadpoints() {
		if lp.GetStatus() == api.StatusC && lp.IsFastChargingActive() {
			return true
		}
	}

	return false
}
