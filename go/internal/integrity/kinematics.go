package integrity

import "math"

func (p *positionChecks) displacement(s Solution, e epoch) Verdict {
	f, r := p.station.Fixed, p.station.Reference
	h, v := math.Hypot(e.enu[0], e.enu[1]), math.Abs(e.enu[2])
	hu, vu := 3*math.Hypot(s.HAccM, r.HorizontalUncertaintyM), 3*math.Hypot(s.VAccM, r.VerticalUncertaintyM)
	out := Verdict{State: Assured, Metrics: map[string]float64{"horizontal_m": h, "vertical_m": v, "horizontal_uncertainty_m": hu, "vertical_uncertainty_m": vu}, Thresholds: map[string]float64{"displacement_m": f.DisplacementM, "vertical_displacement_m": f.VerticalDisplacementM}}
	if f.DisplacementM > 0 && hu > f.DisplacementM || f.VerticalDisplacementM > 0 && vu > f.VerticalDisplacementM {
		out.State = Unavailable
		out.Reasons = []string{ReasonInsufficientAccuracy}
		return out
	}
	if f.DisplacementM > 0 && h-hu > f.DisplacementM {
		out.State = Unassured
		out.Reasons = append(out.Reasons, ReasonHorizontalError)
	}
	if f.VerticalDisplacementM > 0 && v-vu > f.VerticalDisplacementM {
		out.State = Unassured
		out.Reasons = append(out.Reasons, ReasonVerticalError)
	}
	return out
}

func (p *positionChecks) kinematics(s Solution, e epoch, usable bool) Verdict {
	m := p.station.Motion
	if !usable {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix, ReasonInsufficientAccuracy}}
	}
	if m.MaxSpeedAccuracyMPS > 0 && s.SAccMPS > m.MaxSpeedAccuracyMPS {
		return Verdict{State: Unavailable, Reasons: []string{ReasonInsufficientAccuracy}}
	}
	out := Verdict{State: Assured, Metrics: map[string]float64{"speed_mps": e.vel.Norm(), "vertical_speed_mps": math.Abs(s.VelD), "s_acc_mps": s.SAccMPS}, Thresholds: map[string]float64{}}
	missing := false
	test := func(name string, value, uncertainty, limit float64) {
		if limit <= 0 {
			return
		}
		out.Metrics[name] = value
		out.Metrics[name+"_uncertainty"] = uncertainty
		out.Thresholds[name+"_limit"] = limit
		if uncertainty > limit {
			missing = true
			return
		}
		if value-uncertainty > limit {
			out.State = Unassured
			out.Reasons = append(out.Reasons, name)
		}
	}
	if m.ReportedSpeed {
		test("reported_speed", e.vel.Norm(), 3*e.sAcc, p.station.MaxSpeedMPS)
	}
	test("vertical_speed", math.Abs(s.VelD), 3*e.sAcc, m.MaxVerticalSpeedMPS)
	if m.MaxAccelerationMPS2 > 0 || m.MaxLateralAccelerationMPS2 > 0 {
		if len(p.history) == 0 {
			missing = true
		} else {
			prev := p.history[len(p.history)-1]
			dt := float64(e.t-prev.t) / 1000
			gap := 10.0
			if m.Gap > 0 {
				gap = m.Gap.Seconds()
			}
			if dt <= 0 || dt > gap {
				missing = true
			} else {
				dv := e.vel.Sub(prev.vel).Scale(1 / dt)
				unc := 3 * (e.sAcc + prev.sAcc) / dt
				test("acceleration", dv.Norm(), unc, m.MaxAccelerationMPS2)
				// Component normal to the mean horizontal velocity measures turn
				// acceleration without heading wrap or a singularity at a stop.
				mean := e.vel.Add(prev.vel).Scale(.5)
				horizontal := mean.Sub(e.up.Scale(mean.Dot(e.up)))
				if horizontal.Norm() > 3*(e.sAcc+prev.sAcc) {
					along := horizontal.Scale(1 / horizontal.Norm())
					lat := dv.Sub(e.up.Scale(dv.Dot(e.up))).Sub(along.Scale(dv.Dot(along)))
					test("lateral_acceleration", lat.Norm(), unc, m.MaxLateralAccelerationMPS2)
				} else if m.MaxLateralAccelerationMPS2 > 0 && e.vel.Norm() > 3*e.sAcc {
					missing = true
				}
			}
		}
	}
	if missing && out.State == Assured {
		out.State = Unavailable
		out.Reasons = []string{ReasonNoWindow, ReasonInsufficientAccuracy}
	}
	return out
}
