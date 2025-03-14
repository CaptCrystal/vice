// pkg/aviation/aviation.go
// Copyright(c) 2022-2024 vice contributors, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package aviation

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/bits"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mmp/vice/pkg/math"
	"github.com/mmp/vice/pkg/rand"
	"github.com/mmp/vice/pkg/renderer"
	"github.com/mmp/vice/pkg/util"

	"github.com/brunoga/deep"
	"github.com/klauspost/compress/zstd"
	"github.com/mmp/earcut-go"
)

type ReportingPoint struct {
	Fix      string
	Location math.Point2LL
}

type Arrival struct {
	Waypoints       WaypointArray                       `json:"waypoints"`
	RunwayWaypoints map[string]map[string]WaypointArray `json:"runway_waypoints"` // Airport -> runway -> waypoints
	SpawnWaypoint   string                              `json:"spawn"`            // if "waypoints" aren't specified
	CruiseAltitude  float32                             `json:"cruise_altitude"`
	Route           string                              `json:"route"`
	STAR            string                              `json:"star"`

	InitialController   string  `json:"initial_controller"`
	InitialAltitude     float32 `json:"initial_altitude"`
	AssignedAltitude    float32 `json:"assigned_altitude"`
	InitialSpeed        float32 `json:"initial_speed"`
	SpeedRestriction    float32 `json:"speed_restriction"`
	Scratchpad          string  `json:"scratchpad"`
	SecondaryScratchpad string  `json:"secondary_scratchpad"`
	Description         string  `json:"description"`
	CoordinationFix     string  `json:"coordination_fix"`

	ExpectApproach util.OneOf[string, map[string]string] `json:"expect_approach"`

	// Airport -> arrival airlines
	Airlines map[string][]ArrivalAirline `json:"airlines"`
}

type AirlineSpecifier struct {
	ICAO          string   `json:"icao"`
	Fleet         string   `json:"fleet,omitempty"`
	AircraftTypes []string `json:"types,omitempty"`
}

type ArrivalAirline struct {
	AirlineSpecifier
	Airport string `json:"airport"`
}

func (a AirlineSpecifier) Aircraft() []FleetAircraft {
	if a.Fleet == "" && len(a.AircraftTypes) == 0 {
		return DB.Airlines[strings.ToUpper(a.ICAO)].Fleets["default"]
	} else if a.Fleet != "" {
		return DB.Airlines[strings.ToUpper(a.ICAO)].Fleets[a.Fleet]
	} else {
		var f []FleetAircraft
		for _, ty := range a.AircraftTypes {
			f = append(f, FleetAircraft{ICAO: ty, Count: 1})
		}
		return f
	}
}

func (a *AirlineSpecifier) Check(e *util.ErrorLogger) {
	defer e.CheckDepth(e.CurrentDepth())

	e.Push("Airline " + a.ICAO)
	defer e.Pop()

	al, ok := DB.Airlines[strings.ToUpper(a.ICAO)]
	if !ok {
		e.ErrorString("airline not known")
		return
	}

	if a.Fleet == "" && len(a.AircraftTypes) == 0 {
		a.Fleet = "default"
	}
	if a.Fleet != "" {
		if len(a.AircraftTypes) != 0 {
			e.ErrorString("cannot specify both \"fleet\" and \"types\"")
			return
		}
		if _, ok := al.Fleets[a.Fleet]; !ok {
			e.ErrorString("\"fleet\" %s unknown", a.Fleet)
			return
		}
	}

	for _, ac := range a.Aircraft() {
		e.Push("Aircraft " + ac.ICAO)
		if perf, ok := DB.AircraftPerformance[ac.ICAO]; !ok {
			e.ErrorString("aircraft not present in performance database")
		} else {
			if perf.Speed.Min < 35 || perf.Speed.Landing < 35 || perf.Speed.CruiseTAS < 35 ||
				perf.Speed.MaxTAS < 35 || perf.Speed.Min > perf.Speed.MaxTAS {
				e.ErrorString("aircraft's speed specification is questionable: %+v", perf.Speed)
			}
			if perf.Rate.Climb == 0 || perf.Rate.Descent == 0 || perf.Rate.Accelerate == 0 ||
				perf.Rate.Decelerate == 0 {
				e.ErrorString("aircraft's rate specification is questionable: %+v", perf.Rate)
			}
		}
		e.Pop()
	}
}

type Runway struct {
	Id        string
	Heading   float32
	Threshold math.Point2LL
	Elevation int
}

type METAR struct {
	AirportICAO string
	Time        string
	Auto        bool
	Wind        string
	Weather     string
	Altimeter   string
	Rmk         string
}

func (m METAR) String() string {
	auto := ""
	if m.Auto {
		auto = "AUTO"
	}
	return strings.Join([]string{m.AirportICAO, m.Time, auto, m.Wind, m.Weather, m.Altimeter, m.Rmk}, " ")
}

type ATIS struct {
	Airport  string
	AppDep   string
	Code     string
	Contents string
}

// Frequencies are scaled by 1000 and then stored in integers.
type Frequency int

func NewFrequency(f float32) Frequency {
	// 0.5 is key for handling rounding!
	return Frequency(f*1000 + 0.5)
}

func (f Frequency) String() string {
	s := fmt.Sprintf("%03d.%03d", f/1000, f%1000)
	for len(s) < 7 {
		s += "0"
	}
	return s
}

type Controller struct {
	Position           string    // This is the key in the controllers map in JSON
	RadioName          string    `json:"radio_name"`
	Frequency          Frequency `json:"frequency"`
	TCP                string    `json:"sector_id"`       // e.g. N56, 2J, ...
	Scope              string    `json:"scope_char"`      // Optional. If unset, facility id is used for external, last char of sector id for local.
	FacilityIdentifier string    `json:"facility_id"`     // For example the "N" in "N4P" showing the N90 TRACON
	ERAMFacility       bool      `json:"eram_facility"`   // To weed out N56 and N4P being the same fac
	Facility           string    `json:"facility"`        // So we can get the STARS facility from a controller
	DefaultAirport     string    `json:"default_airport"` // only required if CRDA is a thing
}

func (c Controller) Id() string {
	if c.ERAMFacility {
		return c.TCP
	}
	return c.FacilityIdentifier + c.TCP
}

type FlightRules int

const (
	UNKNOWN FlightRules = iota
	IFR
	VFR
	DVFR
	SVFR
)

func (f FlightRules) String() string {
	return [...]string{"Unknown", "IFR", "VFR", "DVFR", "SVFR"}[f]
}

type FlightPlan struct {
	Callsign       string
	Rules          FlightRules
	AircraftType   string
	CruiseSpeed    int
	AssignedSquawk Squawk // from ATC
	// An ECID (CID) are three alpha-numeric characters (eg. 971, 43A,
	// etc.) and is what ERAM assigns to a track to act as another way to
	// identify that track. To execute commands, controllers may use the
	// ECID instead of the aircrafts callsign.
	ECID                   string
	DepartureAirport       string
	DepartTimeEst          int
	DepartTimeActual       int
	Altitude               int
	ArrivalAirport         string
	Hours, Minutes         int
	FuelHours, FuelMinutes int
	AlternateAirport       string
	Exit                   string
	Route                  string
	Remarks                string
}

type FlightStrip struct {
	Callsign    string
	Annotations [9]string
}

type Squawk int

func (s Squawk) String() string { return fmt.Sprintf("%04o", s) }

func ParseSquawk(s string) (Squawk, error) {
	if s == "" {
		return Squawk(0), nil
	}

	sq, err := strconv.ParseInt(s, 8, 32) // base 8!!!
	if err != nil || sq < 0 || sq > 0o7777 {
		return Squawk(0), ErrInvalidSquawkCode
	}
	return Squawk(sq), nil
}

/////////////////////////////////////////////////////////////////////////
// SPC

// SPC (Special Purpose Code) is a unique beacon code,
// indicate an emergency or non-standard operation.
type SPC struct {
	Squawk Squawk
	Code   string
}

var spcs = map[Squawk]string{
	Squawk(0o7400): "LL", // Lost link
	Squawk(0o7500): "HJ", // Hijack/Unlawful Interference
	Squawk(0o7600): "RF", // Communication Failure
	Squawk(0o7700): "EM", // Emergency
	Squawk(0o7777): "MI", // Military interceptor operations
}

func SquawkIsSPC(squawk Squawk) (ok bool, code string) {
	return squawk.IsSPC()
}

// IsSPC returns true if the given squawk code is an SPC.
// The second return value is a string giving the two-letter abbreviated SPC it corresponds to.
func (squawk Squawk) IsSPC() (ok bool, code string) {
	code, ok = spcs[squawk]
	return
}

func StringIsSPC(code string) bool {
	for scpCode := range maps.Values(spcs) {
		if scpCode == code {
			return true
		}
	}
	return false
}

type RadarTrack struct {
	Position    math.Point2LL
	Altitude    int
	Groundspeed int
	Time        time.Time
}

func FormatAltitude(falt float32) string {
	alt := int(falt)
	if alt >= 18000 {
		return "FL" + strconv.Itoa(alt/100)
	} else if alt < 1000 {
		return strconv.Itoa(alt)
	} else {
		th := alt / 1000
		hu := (alt % 1000) / 100 * 100
		if th == 0 {
			return strconv.Itoa(hu)
		} else if hu == 0 {
			return strconv.Itoa(th) + ",000"
		} else {
			return fmt.Sprintf("%d,%03d", th, hu)
		}
	}
}

type TransponderMode int

const (
	Standby  TransponderMode = iota /* off */
	Altitude                        /* mode C */
	On                              /* mode A */
)

func (t TransponderMode) String() string {
	return [...]string{"Standby", "Altitude", "On"}[t]
}

func (fp FlightPlan) BaseType() string {
	s := strings.TrimPrefix(fp.TypeWithoutSuffix(), "H/")
	s = strings.TrimPrefix(s, "S/")
	s = strings.TrimPrefix(s, "J/")
	return s
}

func (fp FlightPlan) TypeWithoutSuffix() string {
	// try to chop off equipment suffix
	actypeFields := strings.Split(fp.AircraftType, "/")
	switch len(actypeFields) {
	case 3:
		// Heavy (presumably), with suffix
		return actypeFields[0] + "/" + actypeFields[1]
	case 2:
		if actypeFields[0] == "H" || actypeFields[0] == "S" || actypeFields[0] == "J" {
			// Heavy or super, no suffix
			return actypeFields[0] + "/" + actypeFields[1]
		} else {
			// No heavy, with suffix
			return actypeFields[0]
		}
	default:
		// Who knows, so leave it alone
		return fp.AircraftType
	}
}

///////////////////////////////////////////////////////////////////////////
// Wind

type Wind struct {
	Direction int32 `json:"direction"`
	Speed     int32 `json:"speed"`
	Gust      int32 `json:"gust"`
}

type WindModel interface {
	GetWindVector(p math.Point2LL, alt float32) [2]float32
	AverageWindVector() [2]float32
}

///////////////////////////////////////////////////////////////////////////

type RadarSite struct {
	Char           string        `json:"char"`
	PositionString string        `json:"position"`
	Position       math.Point2LL // not in JSON, set during deserialize

	Elevation      int32   `json:"elevation"`
	PrimaryRange   int32   `json:"primary_range"`
	SecondaryRange int32   `json:"secondary_range"`
	SlopeAngle     float32 `json:"slope_angle"`
	SilenceAngle   float32 `json:"silence_angle"`
}

func (rs *RadarSite) CheckVisibility(p math.Point2LL, altitude int) (primary, secondary bool, distance float32) {
	// Check altitude first; this is a quick first cull that
	// e.g. takes care of everyone on the ground.
	if altitude <= int(rs.Elevation) {
		return
	}

	// Time to check the angles..
	palt := float32(altitude) * math.FeetToNauticalMiles
	ralt := float32(rs.Elevation) * math.FeetToNauticalMiles
	dalt := palt - ralt
	// not quite true distance, but close enough
	distance = math.NMDistance2LL(rs.Position, p) + math.Abs(palt-ralt)

	// If we normalize the vector from the radar site to the aircraft, then
	// the z (altitude) component gives the cosine of the angle with the
	// "up" direction; in turn, we can check that against the two angles.
	cosAngle := dalt / distance
	// if angle < silence angle, we can't see it, but the test flips since
	// we're testing cosines.
	// FIXME: it's annoying to be repeatedly computing these cosines here...
	if cosAngle > math.Cos(math.Radians(rs.SilenceAngle)) {
		// inside the cone of silence
		return
	}
	// similarly, if angle > 90-slope angle, we can't see it, but again the
	// test flips.
	if cosAngle < math.Cos(math.Radians(90-rs.SlopeAngle)) {
		// below the slope angle
		return
	}

	primary = distance <= float32(rs.PrimaryRange)
	secondary = !primary && distance <= float32(rs.SecondaryRange)
	return
}

type AirspaceVolume struct {
	Name    string             `json:"name"`
	Type    AirspaceVolumeType `json:"type"`
	Floor   int                `json:"floor"`
	Ceiling int                `json:"ceiling"`
	// Polygon
	PolygonBounds *math.Extent2D    // not always set
	Vertices      []math.Point2LL   `json:"vertices"`
	Holes         [][]math.Point2LL `json:"holes"`
	// Circle
	Center math.Point2LL `json:"center"`
	Radius float32       `json:"radius"`
}

type AirspaceVolumeType int

const (
	AirspaceVolumePolygon = iota
	AirspaceVolumeCircle
)

func (t *AirspaceVolumeType) MarshalJSON() ([]byte, error) {
	switch *t {
	case AirspaceVolumePolygon:
		return []byte("\"polygon\""), nil
	case AirspaceVolumeCircle:
		return []byte("\"circle\""), nil
	default:
		return nil, fmt.Errorf("%d: unknown airspace volume type", *t)
	}
}

func (t *AirspaceVolumeType) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "\"polygon\"":
		*t = AirspaceVolumePolygon
		return nil
	case "\"circle\"":
		*t = AirspaceVolumeCircle
		return nil
	default:
		return fmt.Errorf("%s: unknown airspace volume type", string(b))
	}
}

func (a *AirspaceVolume) Inside(p math.Point2LL, alt int) bool {
	if alt <= a.Floor || alt > a.Ceiling {
		return false
	}

	switch a.Type {
	case AirspaceVolumePolygon:
		if a.PolygonBounds != nil && !a.PolygonBounds.Inside(p) {
			return false
		}
		if !math.PointInPolygon2LL(p, a.Vertices) {
			return false
		}
		for _, hole := range a.Holes {
			if math.PointInPolygon2LL(p, hole) {
				return false
			}
		}
		return true
	case AirspaceVolumeCircle:
		return math.NMDistance2LL(p, a.Center) < a.Radius
	default:
		panic("unhandled AirspaceVolume type")
	}
}

func (a *AirspaceVolume) GenerateDrawCommands(cb *renderer.CommandBuffer, nmPerLongitude float32) {
	ld := renderer.GetLinesDrawBuilder()

	switch a.Type {
	case AirspaceVolumePolygon:
		var v [][2]float32
		for _, vtx := range a.Vertices {
			v = append(v, [2]float32(vtx))
		}
		ld.AddLineLoop(v)

		for _, h := range a.Holes {
			var v [][2]float32
			for _, vtx := range h {
				v = append(v, [2]float32(vtx))
			}
			ld.AddLineLoop(v)
		}
	case AirspaceVolumeCircle:
		ld.AddLatLongCircle(a.Center, nmPerLongitude, a.Radius, 360)
	default:
		panic("unhandled AirspaceVolume type")
	}

	ld.GenerateCommands(cb)
	renderer.ReturnLinesDrawBuilder(ld)
}

func FixReadback(fix string) string {
	if aid, ok := DB.Navaids[fix]; ok {
		return util.StopShouting(aid.Name)
	} else {
		return fix
	}
}

func cleanRunway(rwy string) string {
	// The runway may have extra text to distinguish different
	// configurations (e.g., "13.JFK-ILS-13"). Find the prefix that is
	// an actual runway specifier to use in the search below.
	for i, ch := range rwy {
		if ch >= '0' && ch <= '9' {
			continue
		} else if ch == 'L' || ch == 'R' || ch == 'C' {
			return rwy[:i+1]
		} else {
			return rwy[:i]
		}
	}
	return rwy
}

func LookupRunway(icao, rwy string) (Runway, bool) {
	if ap, ok := DB.Airports[icao]; !ok {
		return Runway{}, false
	} else {
		rwy = cleanRunway(rwy)
		idx := slices.IndexFunc(ap.Runways, func(r Runway) bool { return r.Id == rwy })
		if idx == -1 {
			return Runway{}, false
		}
		return ap.Runways[idx], true
	}
}

func LookupOppositeRunway(icao, rwy string) (Runway, bool) {
	if ap, ok := DB.Airports[icao]; !ok {
		return Runway{}, false
	} else {
		rwy = cleanRunway(rwy)

		// Break runway into number and optional extension and swap
		// left/right.
		n := len(rwy)
		num, ext := "", ""
		switch rwy[n-1] {
		case 'R':
			ext = "L"
			num = rwy[:n-1]
		case 'L':
			ext = "R"
			num = rwy[:n-1]
		case 'C':
			ext = "C"
			num = rwy[:n-1]
		default:
			num = rwy
		}

		// Extract the number so we can get the opposite heading
		v, err := strconv.Atoi(num)
		if err != nil {
			return Runway{}, false
		}

		// The (v+18)%36 below would give us 0 for runway 36, so handle 18
		// specially.
		if v == 18 {
			rwy = "36" + ext
		} else {
			rwy = fmt.Sprintf("%d", (v+18)%36) + ext
		}

		idx := slices.IndexFunc(ap.Runways, func(r Runway) bool { return r.Id == rwy })
		if idx == -1 {
			return Runway{}, false
		}
		return ap.Runways[idx], true
	}
}

// returns the ratio of air density at the given altitude (in feet) to the
// air density at sea level, subject to assuming the standard atmosphere.
func DensityRatioAtAltitude(alt float32) float32 {
	altm := alt * 0.3048 // altitude in meters

	// https://en.wikipedia.org/wiki/Barometric_formula#Density_equations
	const g0 = 9.80665    // gravitational constant, m/s^2
	const M_air = 0.02897 // molar mass of earth's air, kg/mol
	const R = 8.314463    // universal gas constant J/(mol K)
	const T_b = 288.15    // reference temperature at sea level, degrees K

	return math.Exp(-g0 * M_air * altm / (R * T_b))
}

func IASToTAS(ias, altitude float32) float32 {
	return ias / math.Sqrt(DensityRatioAtAltitude(altitude))
}

func TASToIAS(tas, altitude float32) float32 {
	return tas * math.Sqrt(DensityRatioAtAltitude(altitude))
}

///////////////////////////////////////////////////////////////////////////
// Arrival

func (ar *Arrival) PostDeserialize(loc Locator, nmPerLongitude float32, magneticVariation float32,
	airports map[string]*Airport, controlPositions map[string]*Controller, e *util.ErrorLogger) {
	defer e.CheckDepth(e.CurrentDepth())

	if ar.Route == "" && ar.STAR == "" {
		e.ErrorString("neither \"route\" nor \"star\" specified")
		return
	}

	if ar.Route != "" {
		e.Push("Route " + ar.Route)
	} else {
		e.Push("Route " + ar.STAR)
	}
	defer e.Pop()

	if len(ar.Waypoints) == 0 {
		// STAR details are coming from the FAA CIFP; make sure
		// everything is ok so we don't get into trouble when we
		// spawn arrivals...
		if ar.STAR == "" {
			e.ErrorString("must provide \"star\" if \"waypoints\" aren't given")
			return
		}
		if ar.SpawnWaypoint == "" {
			e.ErrorString("must specify \"spawn\" if \"waypoints\" aren't given with arrival")
			return
		}

		spawnPoint, spawnTString, ok := strings.Cut(ar.SpawnWaypoint, "@")
		spawnT := float32(0)
		if ok {
			if st, err := strconv.ParseFloat(spawnTString, 32); err != nil {
				e.ErrorString("error parsing spawn offset %q: %s", spawnTString, err)
			} else {
				spawnT = float32(st)
			}
		}

		for icao := range ar.Airlines {
			airport, ok := DB.Airports[icao]
			if !ok {
				e.ErrorString("airport %q not found in database", icao)
				continue
			}

			star, ok := airport.STARs[ar.STAR]
			if !ok {
				e.ErrorString(
					"STAR %q not available for %s. Options: %s",
					ar.STAR, icao, strings.Join(util.SortedMapKeys(airport.STARs), ", "),
				)
				continue
			}

			star.Check(e)

			if len(ar.Waypoints) == 0 {
				for _, tr := range util.SortedMapKeys(star.Transitions) {
					wps := star.Transitions[tr]
					if idx := slices.IndexFunc(wps, func(w Waypoint) bool { return w.Fix == spawnPoint }); idx != -1 {
						if idx == len(wps)-1 {
							e.ErrorString(
								"Only have one waypoint on STAR: %q. 2 or more are necessary for navigation",
								wps[idx].Fix,
							)
						}

						ar.Waypoints = util.DuplicateSlice(wps[idx:])
						ar.Waypoints.InitializeLocations(loc, nmPerLongitude, magneticVariation, e)

						if len(ar.Waypoints) >= 2 && spawnT != 0 {
							ar.Waypoints[0].Location = math.Lerp2f(
								spawnT, ar.Waypoints[0].Location, ar.Waypoints[1].Location,
							)
							ar.Waypoints[0].Fix = "_" + ar.Waypoints[0].Fix
						}

						break
					}
				}
			}

			if star.RunwayWaypoints != nil {
				if ar.RunwayWaypoints == nil {
					ar.RunwayWaypoints = make(map[string]map[string]WaypointArray)
				}
				if ar.RunwayWaypoints[icao] == nil {
					ar.RunwayWaypoints[icao] = make(map[string]WaypointArray)
				}

				for _, rwy := range airport.Runways {
					for starRwy, wp := range star.RunwayWaypoints {
						// Trim leading 0, if any
						if starRwy[0] == '0' {
							starRwy = starRwy[1:]
						}

						n := len(starRwy)
						if starRwy == rwy.Id ||
							(n == len(rwy.Id) && starRwy[n-1] == 'B' /* both */ && starRwy[:n-1] == rwy.Id[:n-1]) {
							ar.RunwayWaypoints[icao][rwy.Id] = util.DuplicateSlice(wp)
							ar.RunwayWaypoints[icao][rwy.Id].InitializeLocations(
								loc, nmPerLongitude, magneticVariation, e,
							)
							break
						}
					}
				}
			}
		}
		switch len(ar.Waypoints) {
		case 0:
			e.ErrorString("Couldn't find waypoint %s in any of the STAR routes", spawnPoint)
			return

		case 1:
			ar.Waypoints[0].HumanHandoff = true // empty string -> to human

		default:
			// add a handoff point randomly halfway between the first two waypoints.
			mid := Waypoint{
				Fix: "_handoff",
				// FIXME: it's a little sketchy to lerp Point2ll coordinates
				// but probably ok over short distances here...
				Location:     math.Lerp2f(0.5, ar.Waypoints[0].Location, ar.Waypoints[1].Location),
				HumanHandoff: true,
			}
			ar.Waypoints = append([]Waypoint{ar.Waypoints[0], mid}, ar.Waypoints[1:]...)
		}
	} else {
		if len(ar.Waypoints) < 2 {
			e.ErrorString(
				"must provide at least two \"waypoints\" for arrival " +
					"(even if \"runway_waypoints\" are provided)",
			)
		}

		ar.Waypoints.InitializeLocations(loc, nmPerLongitude, magneticVariation, e)

		for ap, rwywp := range ar.RunwayWaypoints {
			e.Push("Airport " + ap)

			if _, ok := DB.Airports[ap]; !ok {
				e.ErrorString("airport is unknown")
				continue
			}

			for rwy, wp := range rwywp {
				e.Push("Runway " + rwy)

				if _, ok := LookupRunway(ap, rwy); !ok {
					e.ErrorString("runway %q is unknown. Options: %s", rwy, DB.Airports[ap].ValidRunways())
				}

				wp.InitializeLocations(loc, nmPerLongitude, magneticVariation, e)

				for i := range wp {
					wp[i].OnSTAR = true
				}

				if wp[0].Fix != ar.Waypoints[len(ar.Waypoints)-1].Fix {
					e.ErrorString(
						"initial \"runway_waypoints\" fix must match " +
							"last \"waypoints\" fix",
					)
				}

				// For the check, splice together the last common
				// waypoint and the runway waypoints.  This will give
				// us a repeated first fix, but this way we can check
				// compliance with restrictions at that fix...
				ewp := append([]Waypoint{ar.Waypoints[len(ar.Waypoints)-1]}, wp...)
				approachAssigned := ar.ExpectApproach.A != nil || ar.ExpectApproach.B != nil
				WaypointArray(ewp).CheckArrival(e, controlPositions, approachAssigned)

				e.Pop()
			}
			e.Pop()
		}
	}

	for i := range ar.Waypoints {
		ar.Waypoints[i].OnSTAR = true
	}

	approachAssigned := ar.ExpectApproach.A != nil || ar.ExpectApproach.B != nil
	ar.Waypoints.CheckArrival(e, controlPositions, approachAssigned)

	for arrivalAirport, airlines := range ar.Airlines {
		e.Push("Arrival airport " + arrivalAirport)
		if len(airlines) == 0 {
			e.ErrorString("no \"airlines\" specified for arrivals to " + arrivalAirport)
		}
		for _, al := range airlines {
			al.Check(e)
			if _, ok := DB.Airports[al.Airport]; !ok {
				e.ErrorString("departure airport \"airport\" %q unknown", al.Airport)
			}
		}

		_, ok := airports[arrivalAirport]
		if !ok {
			e.ErrorString("arrival airport %q unknown", arrivalAirport)
		}

		e.Pop()
	}

	if ar.ExpectApproach.A != nil { // Given a single string
		if len(ar.Airlines) > 1 {
			e.ErrorString("There are multiple arrival airports but only one approach in \"expect_approach\"")
		}
		// Ugly way to get the key from a one-element map
		var airport string
		for airport, _ = range ar.Airlines {
		}
		// We checked the arrival airports were valid above, no need to issue an error if not found.
		if ap, ok := airports[airport]; ok {
			if _, ok := ap.Approaches[*ar.ExpectApproach.A]; !ok {
				e.ErrorString(
					"arrival airport %q doesn't have a %q approach for \"expect_approach\"",
					airport, *ar.ExpectApproach.A,
				)
			}
		}
	} else if ar.ExpectApproach.B != nil {
		for airport, appr := range *ar.ExpectApproach.B {
			if _, ok := ar.Airlines[airport]; !ok {
				e.ErrorString(
					"airport %q is listed in \"expect_approach\" but is not in arrival airports",
					airport,
				)
			} else if ap, ok := airports[airport]; ok {
				if _, ok := ap.Approaches[appr]; !ok {
					e.ErrorString(
						"arrival airport %q doesn't have a %q approach for \"expect_approach\"",
						airport, appr,
					)
				}
			}
		}
	}

	if ar.InitialAltitude == 0 {
		e.ErrorString("must specify \"initial_altitude\"")
	} else {
		// Make sure the initial altitude isn't below any of
		// altitude restrictions.
		for _, wp := range ar.Waypoints {
			if wp.AltitudeRestriction != nil &&
				wp.AltitudeRestriction.TargetAltitude(ar.InitialAltitude) > ar.InitialAltitude {
				e.ErrorString("\"initial_altitude\" is below altitude restriction at %q", wp.Fix)
			}
		}
	}

	if ar.InitialSpeed == 0 {
		e.ErrorString("must specify \"initial_speed\"")
	}

	if ar.InitialController == "" {
		e.ErrorString("\"initial_controller\" missing")
	} else if _, ok := controlPositions[ar.InitialController]; !ok {
		e.ErrorString("controller %q not found for \"initial_controller\"", ar.InitialController)
	}

	for id, controller := range controlPositions {
		if controller.ERAMFacility && controller.FacilityIdentifier == "" {
			e.ErrorString("%q is an ERAM facility, but has no facility id specified", id)
		}
	}
}

func (a Arrival) GetRunwayWaypoints(airport, rwy string) WaypointArray {
	if ap, ok := a.RunwayWaypoints[airport]; !ok {
		return nil
	} else if wp, ok := ap[rwy]; !ok {
		return nil
	} else {
		return wp
	}
}

///////////////////////////////////////////////////////////////////////////

// Note: this should match ViceMapSpec/VideoMap in crc2vice/dat2vice. (crc2vice
// doesn't support all of these, though.)
type VideoMap struct {
	Label       string // for DCB
	Group       int    // 0 -> A, 1 -> B
	Name        string // For maps system list
	Id          int
	Category    int
	Restriction struct {
		Id        int
		Text      [2]string
		TextBlink bool
		HideText  bool
	}
	Color int
	Lines [][]math.Point2LL

	CommandBuffer renderer.CommandBuffer
}

// This should match VideoMapLibrary in dat2vice
type VideoMapLibrary struct {
	Maps []VideoMap
}

// VideoMapManifest stores which maps are available in a video map file and
// is also able to provide the video map file's hash.
type VideoMapManifest struct {
	names      map[string]interface{}
	filesystem fs.FS
	filename   string
}

func CheckVideoMapManifest(filename string, e *util.ErrorLogger) {
	defer e.CheckDepth(e.CurrentDepth())

	manifest, err := LoadVideoMapManifest(filename)
	if err != nil {
		e.Error(err)
		return
	}

	vms, err := LoadVideoMapLibrary(filename)
	if err != nil {
		e.Error(err)
		return
	}

	for n := range manifest.names {
		if !slices.ContainsFunc(vms.Maps, func(v VideoMap) bool { return v.Name == n }) {
			e.ErrorString("%s: map is in manifest file but not video map file", n)
		}
	}
	for _, m := range vms.Maps {
		if _, ok := manifest.names[m.Name]; !ok {
			e.ErrorString("%s: map is in video map file but not manifest", m.Name)
		}
	}
}

func LoadVideoMapManifest(filename string) (*VideoMapManifest, error) {
	filesystem := videoMapFS(filename)

	// Load the manifest and do initial error checking
	mf, _ := strings.CutSuffix(filename, ".zst")
	mf, _ = strings.CutSuffix(mf, "-videomaps.gob")
	mf += "-manifest.gob"

	fm, err := filesystem.Open(mf)
	if err != nil {
		return nil, err
	}
	defer fm.Close()

	var names map[string]interface{}
	dec := gob.NewDecoder(fm)
	if err := dec.Decode(&names); err != nil {
		return nil, err
	}

	// Make sure the file exists but don't load it until it's needed.
	f, err := filesystem.Open(filename)
	if err != nil {
		return nil, err
	} else {
		f.Close()
	}

	return &VideoMapManifest{
		names:      names,
		filesystem: filesystem,
		filename:   filename,
	}, nil
}

func (v VideoMapManifest) HasMap(s string) bool {
	_, ok := v.names[s]
	return ok
}

// Hash returns a hash of the underlying video map file (i.e., not the manifest!)
func (v VideoMapManifest) Hash() ([]byte, error) {
	if f, err := v.filesystem.Open(v.filename); err == nil {
		defer f.Close()
		return util.Hash(f)
	} else {
		return nil, err
	}
}

func LoadVideoMapLibrary(path string) (*VideoMapLibrary, error) {
	filesystem := videoMapFS(path)
	f, err := filesystem.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	contents, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	var r io.Reader
	br := bytes.NewReader(contents)
	var zr *zstd.Decoder
	if len(contents) > 4 && contents[0] == 0x28 && contents[1] == 0xb5 && contents[2] == 0x2f && contents[3] == 0xfd {
		// zstd compressed
		zr, _ = zstd.NewReader(br, zstd.WithDecoderConcurrency(0))
		defer zr.Close()
		r = zr
	} else {
		r = br
	}

	// Decode the gobfile.
	var vmf VideoMapLibrary
	if err := gob.NewDecoder(r).Decode(&vmf); err != nil {
		// Try the old format, just an array of maps
		_, _ = br.Seek(0, io.SeekStart)
		if zr != nil {
			_ = zr.Reset(br)
		}
		if err := gob.NewDecoder(r).Decode(&vmf.Maps); err != nil {
			return nil, err
		}
	}

	// Convert the line specifications into command buffers for drawing.
	ld := renderer.GetLinesDrawBuilder()
	defer renderer.ReturnLinesDrawBuilder(ld)
	for i, m := range vmf.Maps {
		ld.Reset()

		for _, lines := range m.Lines {
			// Slightly annoying: the line vertices are stored with
			// Point2LLs but AddLineStrip() expects [2]float32s.
			fl := util.MapSlice(lines, func(p math.Point2LL) [2]float32 { return p })
			ld.AddLineStrip(fl)
		}
		ld.GenerateCommands(&m.CommandBuffer)

		// Clear out Lines so that the memory can be reclaimed since they
		// aren't needed any more.
		m.Lines = nil
		vmf.Maps[i] = m
	}

	return &vmf, nil
}

// Loads the specified video map file, though only if its hash matches the
// provided hash. Returns an error otherwise.
func HashCheckLoadVideoMap(path string, wantHash []byte) (*VideoMapLibrary, error) {
	filesystem := videoMapFS(path)
	f, err := filesystem.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if gotHash, err := util.Hash(f); err != nil {
		return nil, err
	} else if !slices.Equal(gotHash, wantHash) {
		return nil, errors.New("hash mismatch")
	}

	return LoadVideoMapLibrary(path)
}

// Returns an fs.FS that allows us to load the video map with the given path.
func videoMapFS(path string) fs.FS {
	if filepath.IsAbs(path) {
		return util.RootFS{}
	} else {
		return util.GetResourcesFS()
	}
}

func PrintVideoMaps(path string, e *util.ErrorLogger) {
	if vmf, err := LoadVideoMapLibrary(path); err != nil {
		e.Error(err)
		return
	} else {
		sort.Slice(
			vmf.Maps, func(i, j int) bool {
				vi, vj := vmf.Maps[i], vmf.Maps[j]
				if vi.Id != vj.Id {
					return vi.Id < vj.Id
				}
				return vi.Name < vj.Name
			},
		)

		fmt.Printf("%5s\t%20s\t%s\n", "Id", "Label", "Name")
		for _, m := range vmf.Maps {
			fmt.Printf("%5d\t%20s\t%s\n", m.Id, m.Label, m.Name)
		}
	}
}

///////////////////////////////////////////////////////////////////////////

// split -> config
type SplitConfigurationSet map[string]SplitConfiguration

// callsign -> controller contig
type SplitConfiguration map[string]*MultiUserController

type MultiUserController struct {
	Primary          bool     `json:"primary"`
	BackupController string   `json:"backup"`
	Departures       []string `json:"departures"`
	Arrivals         []string `json:"arrivals"` // TEMPORARY for inbound flows transition
	InboundFlows     []string `json:"inbound_flows"`
}

///////////////////////////////////////////////////////////////////////////
// SplitConfigurations

func (sc SplitConfigurationSet) GetConfiguration(split string) (SplitConfiguration, error) {
	if len(sc) == 1 {
		// ignore split
		for _, config := range sc {
			return config, nil
		}
	}

	config, ok := sc[split]
	if !ok {
		return nil, fmt.Errorf("%s: split not found", split)
	}
	return config, nil
}

func (sc SplitConfigurationSet) GetPrimaryController(split string) (string, error) {
	configs, err := sc.GetConfiguration(split)
	if err != nil {
		return "", err
	}

	for callsign, mc := range configs {
		if mc.Primary {
			return callsign, nil
		}
	}

	return "", fmt.Errorf("No primary controller in split")
}

func (sc SplitConfigurationSet) Len() int {
	return len(sc)
}

func (sc SplitConfigurationSet) Splits() []string {
	return util.SortedMapKeys(sc)
}

///////////////////////////////////////////////////////////////////////////
// SplitConfiguration

// ResolveController takes a controller callsign and returns the signed-in
// controller that is responsible for that position (possibly just the
// provided callsign).
func (sc SplitConfiguration) ResolveController(id string, active func(id string) bool) (string, error) {
	origId := id
	i := 0
	for {
		if ctrl, ok := sc[id]; !ok {
			return "", fmt.Errorf("%s: failed to find controller in MultiControllers", id)
		} else if ctrl.Primary || active(id) {
			return id, nil
		} else {
			id = ctrl.BackupController
		}

		i++
		if i == 20 {
			return "", fmt.Errorf("%s: unable to find controller backup", origId)
		}
	}
}

func (sc SplitConfiguration) GetInboundController(group string) (string, error) {
	for callsign, ctrl := range sc {
		if ctrl.IsInboundController(group) {
			return callsign, nil
		}
	}

	return "", fmt.Errorf("%s: couldn't find inbound controller", group)
}

func (sc SplitConfiguration) GetDepartureController(airport, runway, sid string) (string, error) {
	for callsign, ctrl := range sc {
		if ctrl.IsDepartureController(airport, runway, sid) {
			return callsign, nil
		}
	}

	return "", fmt.Errorf("%s/%s: couldn't find departure controller", airport, sid)
}

///////////////////////////////////////////////////////////////////////////
// MultiUserController

func (c *MultiUserController) IsDepartureController(ap, rwy, sid string) bool {
	for _, d := range c.Departures {
		depAirport, depSIDRwy, ok := strings.Cut(d, "/")
		if ok { // have a runway or SID
			if ap == depAirport && (rwy == depSIDRwy || sid == depSIDRwy) {
				return true
			}
		} else { // no runway/SID, so only match airport
			if ap == depAirport {
				return true
			}
		}
	}
	return false
}

func (c *MultiUserController) IsInboundController(group string) bool {
	return slices.Contains(c.InboundFlows, group)
}

///////////////////////////////////////////////////////////////////////////
// SquawkCodePool

type SquawkCodePool struct {
	First, Last Squawk // inclusive range of codes
	// Available squawk codes are represented by a bitset
	AssignedBits []uint64
}

func makePool(first, last int) *SquawkCodePool {
	ncodes := last - first + 1
	nalloc := (ncodes + 63) / 64

	p := &SquawkCodePool{
		First:        Squawk(first),
		Last:         Squawk(last),
		AssignedBits: make([]uint64, nalloc),
	}

	p.removeInvalidCodes()

	// Mark the excess invalid codes in the last entry of AssignedBits as
	// taken so that we don't try to assign them later.
	slop := ncodes % 64
	p.AssignedBits[nalloc-1] |= ^uint64(0) << slop

	return p
}

func (p *SquawkCodePool) removeInvalidCodes() {
	// Remove the non-discrete codes (i.e., ones ending in 00).
	for i := 0; i <= 0o7700; i += 0o100 {
		_ = p.Claim(Squawk(i))
	}

	claimRange := func(start, end int) {
		for i := start; i < end; i++ {
			_ = p.Claim(Squawk(i))
		}
	}
	claimBlock := func(start int) {
		claimRange(start, start+64)
	}

	// Remove various reserved squawk codes, per 7110.66G
	// https://www.faa.gov/documentLibrary/media/Order/FAA_Order_JO_7110.66G_NBCAP.pdf.
	_ = p.Claim(0o1200)
	_ = p.Claim(0o1201)
	_ = p.Claim(0o1202)
	_ = p.Claim(0o1205)
	_ = p.Claim(0o1206)
	claimRange(0o1207, 0o1233)
	claimRange(0o1235, 0o1254)
	claimRange(0o1256, 0o1272)
	_ = p.Claim(0o1234)
	_ = p.Claim(0o1255)
	claimRange(0o1273, 0o1275)
	_ = p.Claim(0o1276)
	_ = p.Claim(0o1277)
	_ = p.Claim(0o2000)
	claimRange(0o4400, 0o4433)
	claimRange(0o4434, 0o4437)
	claimRange(0o4440, 0o4452)
	_ = p.Claim(0o4453)
	claimRange(0o4454, 0o4477)
	_ = p.Claim(0o7400)
	claimRange(0o7501, 0o7577)
	_ = p.Claim(0o7500)
	_ = p.Claim(0o7600)
	claimRange(0o7601, 0o7607)
	_ = p.Claim(0o7700)
	claimRange(0o7701, 0o7707)
	_ = p.Claim(0o7777)

	// TODO? 0100, 0200, 0300, 0400 blocks?

	// FIXME: these probably shouldn't be hardcoded like this but should be available to PCT.
	claimBlock(0o5100) // PCT TRACON for DC SFRA/FRZ
	claimBlock(0o5200) // PCT TRACON for DC SFRA/FRZ

	claimBlock(0o5000)
	claimBlock(0o5400)
	claimBlock(0o6100)
	claimBlock(0o6400)

	_ = p.Claim(0o7777)
	for squawk := range spcs {
		_ = p.Claim(squawk)
	}
}

func MakeCompleteSquawkCodePool() *SquawkCodePool {
	return makePool(0o1001, 0o7777)
}

func MakeSquawkBankCodePool(bank int) *SquawkCodePool {
	return makePool(bank*0o100+1, bank*0o100+0o77)
}

func (p *SquawkCodePool) Get() (Squawk, error) {
	start := rand.Intn(len(p.AssignedBits)) // random starting point in p.AssignedBits
	rot := rand.Intn(64)                    // random rotation to randomize search start within each uint64

	for i := range len(p.AssignedBits) {
		// Start the search at start, then wrap around.
		idx := (start + i) % len(p.AssignedBits)

		if p.AssignedBits[idx] == ^uint64(0) {
			// All are assigned in this chunk of 64 squawk codes.
			continue
		}

		// Flip it around and see which ones are available.
		available := ^p.AssignedBits[idx]

		// Randomly rotate the bits so that when we start searching for a
		// set bit starting from the low bit, we effectively randomize
		// which bit index we're starting from.
		available = bits.RotateLeft64(available, rot)

		// Find the last set bit and then map that back to a bit index in
		// the unrotated bits.
		bit := (bits.TrailingZeros64(available) + 64 - rot) % 64

		// Record that we've taken it
		p.AssignedBits[idx] |= (1 << bit)

		return p.First + Squawk(64*idx+bit), nil
	}

	return Squawk(0), ErrNoMoreAvailableSquawkCodes
}

func (p *SquawkCodePool) indices(code Squawk) (int, int, error) {
	if code < p.First || code > p.Last {
		return 0, 0, ErrSquawkCodeNotManagedByPool
	}
	offset := int(code - p.First)
	return offset / 64, offset % 64, nil
}

func (p *SquawkCodePool) IsAssigned(code Squawk) bool {
	if idx, bit, err := p.indices(code); err == nil {
		return p.AssignedBits[idx]&(1<<bit) != 0
	}
	return false
}

func (p *SquawkCodePool) Return(code Squawk) error {
	if !p.IsAssigned(code) {
		return ErrSquawkCodeUnassigned
	}
	if idx, bit, err := p.indices(code); err != nil {
		return err
	} else {
		// Clear the bit
		p.AssignedBits[idx] &= ^(1 << bit)
		return nil
	}
}

func (p *SquawkCodePool) Claim(code Squawk) error {
	if p.IsAssigned(code) {
		return ErrSquawkCodeAlreadyAssigned
	}
	if idx, bit, err := p.indices(code); err != nil {
		return err
	} else {
		// Set the bit
		p.AssignedBits[idx] |= (1 << bit)
		return nil
	}
}

func (p *SquawkCodePool) NumAvailable() int {
	n := int(p.Last - p.First + 1) // total possible
	for _, b := range p.AssignedBits {
		// Reduce the count based on how many are assigned.
		n -= bits.OnesCount64(b)
	}
	return n
}

type ControllerAirspaceVolume struct {
	LowerLimit    int               `json:"lower"`
	UpperLimit    int               `json:"upper"`
	Boundaries    [][]math.Point2LL `json:"boundary_polylines"` // not in JSON
	BoundaryNames []string          `json:"boundaries"`
	Label         string            `json:"label"`
	LabelPosition math.Point2LL     `json:"label_position"`
}

///////////////////////////////////////////////////////////////////////////
// RestrictionArea

// This many adapted and then this many user-defined
const MaxRestrictionAreas = 100

type RestrictionArea struct {
	Title        string        `json:"title"`
	Text         [2]string     `json:"text"`
	BlinkingText bool          `json:"blinking_text"`
	HideId       bool          `json:"hide_id"`
	TextPosition math.Point2LL `json:"text_position"`
	CircleCenter math.Point2LL `json:"circle_center"`
	CircleRadius float32       `json:"circle_radius"`
	VerticesUser WaypointArray `json:"vertices"`
	Vertices     [][]math.Point2LL
	Closed       bool `json:"closed"`
	Shaded       bool `json:"shade_region"`
	Color        int  `json:"color"`

	Tris    [][3]math.Point2LL
	Deleted bool
}

type Airspace struct {
	Boundaries map[string][]math.Point2LL            `json:"boundaries"`
	Volumes    map[string][]ControllerAirspaceVolume `json:"volumes"`
}

func RestrictionAreaFromTFR(tfr TFR) RestrictionArea {
	ra := RestrictionArea{
		Title:    tfr.LocalName,
		Vertices: deep.MustCopy(tfr.Points),
	}

	if len(ra.Title) > 32 {
		ra.Title = ra.Title[:32]
	}

	ra.HideId = true
	ra.Closed = true
	ra.Shaded = true // ??
	ra.TextPosition = ra.AverageVertexPosition()

	ra.UpdateTriangles()

	return ra
}

func (ra *RestrictionArea) AverageVertexPosition() math.Point2LL {
	var c math.Point2LL
	var n float32
	for _, loop := range ra.Vertices {
		n += float32(len(loop))
		for _, v := range loop {
			c = math.Add2f(c, v)
		}
	}
	return math.Scale2f(c, math.Max(1, 1/n)) // avoid 1/0 and return (0,0) if there are no verts.
}

func (ra *RestrictionArea) UpdateTriangles() {
	if !ra.Closed || !ra.Shaded {
		ra.Tris = nil
		return
	}

	clear(ra.Tris)
	for _, loop := range ra.Vertices {
		if len(loop) < 3 {
			continue
		}

		vertices := make([]earcut.Vertex, len(loop))
		for i, v := range loop {
			vertices[i].P = [2]float64{float64(v[0]), float64(v[1])}
		}

		for _, tri := range earcut.Triangulate(earcut.Polygon{Rings: [][]earcut.Vertex{vertices}}) {
			var v32 [3]math.Point2LL
			for i, v64 := range tri.Vertices {
				v32[i] = [2]float32{float32(v64.P[0]), float32(v64.P[1])}
			}
			ra.Tris = append(ra.Tris, v32)
		}
	}
}

func (ra *RestrictionArea) MoveTo(p math.Point2LL) {
	if ra.CircleRadius > 0 {
		// Circle
		delta := math.Sub2f(p, ra.CircleCenter)
		ra.CircleCenter = p
		ra.TextPosition = math.Add2f(ra.TextPosition, delta)
	} else {
		pc := ra.TextPosition
		if pc.IsZero() {
			pc = ra.AverageVertexPosition()
		}
		delta := math.Sub2f(p, pc)
		ra.TextPosition = p

		for _, loop := range ra.Vertices {
			for i := range loop {
				loop[i] = math.Add2f(loop[i], delta)
			}
		}
	}
}

type STARSFacilityAdaptation struct {
	AirspaceAwareness   []AirspaceAwareness               `json:"airspace_awareness"`
	ForceQLToSelf       bool                              `json:"force_ql_self"`
	AllowLongScratchpad bool                              `json:"allow_long_scratchpad"`
	VideoMapNames       []string                          `json:"stars_maps"`
	VideoMapLabels      map[string]string                 `json:"map_labels"`
	ControllerConfigs   map[string]*STARSControllerConfig `json:"controller_configs"`
	InhibitCAVolumes    []AirspaceVolume                  `json:"inhibit_ca_volumes"`
	RadarSites          map[string]*RadarSite             `json:"radar_sites"`
	Center              math.Point2LL                     `json:"-"`
	CenterString        string                            `json:"center"`
	Range               float32                           `json:"range"`
	Scratchpads         map[string]string                 `json:"scratchpads"`
	SignificantPoints   map[string]SignificantPoint       `json:"significant_points"`
	Altimeters          []string                          `json:"altimeters"`

	MonitoredBeaconCodeBlocksString *string `json:"beacon_code_blocks"`
	MonitoredBeaconCodeBlocks       []Squawk

	VideoMapFile      string                     `json:"video_map_file"`
	CoordinationFixes map[string]AdaptationFixes `json:"coordination_fixes"`
	SingleCharAIDs    map[string]string          `json:"single_char_aids"` // Char to airport
	BeaconBank        int                        `json:"beacon_bank"`
	KeepLDB           bool                       `json:"keep_ldb"`
	FullLDBSeconds    int                        `json:"full_ldb_seconds"`

	HandoffAcceptFlashDuration int  `json:"handoff_acceptance_flash_duration"`
	DisplayHOFacilityOnly      bool `json:"display_handoff_facility_only"`
	HOSectorDisplayDuration    int  `json:"handoff_sector_display_duration"`

	PDB struct {
		ShowScratchpad2   bool `json:"show_scratchpad2"`
		HideGroundspeed   bool `json:"hide_gs"`
		ShowAircraftType  bool `json:"show_aircraft_type"`
		SplitGSAndCWT     bool `json:"split_gs_and_cwt"`
		DisplayCustomSPCs bool `json:"display_custom_spcs"`
	} `json:"pdb"`
	Scratchpad1 struct {
		DisplayExitFix     bool `json:"display_exit_fix"`
		DisplayExitFix1    bool `json:"display_exit_fix_1"`
		DisplayExitGate    bool `json:"display_exit_gate"`
		DisplayAltExitGate bool `json:"display_alternate_exit_gate"`
	} `json:"scratchpad1"`
	CustomSPCs []string `json:"custom_spcs"`

	CoordinationLists []CoordinationList `json:"coordination_lists"`
	RestrictionAreas  []RestrictionArea  `json:"restriction_areas"`
	UseLegacyFont     bool               `json:"use_legacy_font"`
}

type STARSControllerConfig struct {
	VideoMapNames                   []string      `json:"video_maps"`
	DefaultMaps                     []string      `json:"default_maps"`
	Center                          math.Point2LL `json:"-"`
	CenterString                    string        `json:"center"`
	Range                           float32       `json:"range"`
	MonitoredBeaconCodeBlocksString *string       `json:"beacon_code_blocks"`
	MonitoredBeaconCodeBlocks       []Squawk
}

type CoordinationList struct {
	Name          string   `json:"name"`
	Id            string   `json:"id"`
	Airports      []string `json:"airports"`
	YellowEntries bool     `json:"yellow_entries"`
}

type SignificantPoint struct {
	Name         string        // JSON comes in as a map from name to SignificantPoint; we set this.
	ShortName    string        `json:"short_name"`
	Abbreviation string        `json:"abbreviation"`
	Description  string        `json:"description"`
	Location     math.Point2LL `json:"location"`
}

type AirspaceAwareness struct {
	Fix                 []string `json:"fixes"`
	AltitudeRange       [2]int   `json:"altitude_range"`
	ReceivingController string   `json:"receiving_controller"`
	AircraftType        []string `json:"aircraft_type"`
}

type InboundFlow struct {
	Arrivals    []Arrival    `json:"arrivals"`
	Overflights []Overflight `json:"overflights"`
}

func (fa *STARSFacilityAdaptation) GetCoordinationFix(fp *STARSFlightPlan, acpos math.Point2LL, waypoints []Waypoint) (string, bool) {
	for fix, adaptationFixes := range fa.CoordinationFixes {
		if adaptationFix, err := adaptationFixes.Fix(fp.Altitude); err == nil {
			if adaptationFix.Type == ZoneBasedFix {
				// Exclude zone based fixes for now. They come in after the route-based fix
				continue
			}

			// FIXME (as elsewhere): make this more robust
			if strings.Contains(fp.Route, fix) {
				return fix, true
			}

			// FIXME: why both this and checking fp.Route?
			for _, waypoint := range waypoints {
				if waypoint.Fix == fix {
					return fix, true
				}
			}
		}

	}

	var closestFix string
	minDist := float32(1e30)
	for fix, adaptationFixes := range fa.CoordinationFixes {
		for _, adaptationFix := range adaptationFixes {
			if adaptationFix.Type == ZoneBasedFix {
				if loc, ok := DB.LookupWaypoint(fix); !ok {
					// FIXME: check this (if it isn't already) at scenario load time.
					panic(fix + ": not found in fixes database")
				} else if dist := math.NMDistance2LL(acpos, loc); dist < minDist {
					minDist = dist
					closestFix = fix
				}
			}
		}
	}

	return closestFix, closestFix != ""
}

type STARSFlightPlan struct {
	*FlightPlan
	FlightPlanType      int
	CoordinationTime    CoordinationTime
	CoordinationFix     string
	ContainedFacilities []string
	Altitude            string
	SP1                 string
	SP2                 string
	InitialController   string // For abbreviated FPs
}

type CoordinationTime struct {
	Time time.Time
	Type string // A for arrivals, P for Departures, E for overflights
}

// Flight plan types (STARS)
const (
	// Flight plan received from a NAS ARTCC.  This is a flight plan that
	// has been sent over by an overlying ERAM facility.
	RemoteEnroute = iota

	// Flight plan received from an adjacent terminal facility This is a
	// flight plan that has been sent over by another STARS facility.
	RemoteNonEnroute

	// VFR interfacility flight plan entered locally for which the NAS
	// ARTCC has not returned a flight plan This is a flight plan that is
	// made by a STARS facility that gets a NAS code.
	LocalEnroute

	// Flight plan entered by TCW or flight plan from an adjacent terminal
	// that has been handed off to this STARS facility This is a flight
	// plan that is made at a STARS facility and gets a local code.
	LocalNonEnroute
)

func MakeSTARSFlightPlan(fp *FlightPlan) *STARSFlightPlan {
	return &STARSFlightPlan{
		FlightPlan: fp,
		Altitude:   fmt.Sprint(fp.Altitude),
	}
}

func (fp *STARSFlightPlan) SetCoordinationFix(fa STARSFacilityAdaptation, ac *Aircraft, simTime time.Time) error {
	cf, ok := fa.GetCoordinationFix(fp, ac.Position(), ac.Waypoints())
	if !ok {
		return ErrNoCoordinationFix
	}
	fp.CoordinationFix = cf

	if dist, err := ac.DistanceAlongRoute(cf); err == nil {
		m := dist / float32(fp.CruiseSpeed) * 60
		fp.CoordinationTime = CoordinationTime{
			Time: simTime.Add(time.Duration(m * float32(time.Minute))),
		}
	} else { // zone based fixes.
		loc, ok := DB.LookupWaypoint(fp.CoordinationFix)
		if !ok {
			return ErrNoCoordinationFix
		}

		dist := math.NMDistance2LL(ac.Position(), loc)
		m := dist / float32(fp.CruiseSpeed) * 60
		fp.CoordinationTime = CoordinationTime{
			Time: simTime.Add(time.Duration(m * float32(time.Minute))),
		}
	}
	return nil
}

///////////////////////////////////////////////////////////////////////////
// Airspace

func InAirspace(p math.Point2LL, alt float32, volumes []ControllerAirspaceVolume) (bool, [][2]int) {
	var altRanges [][2]int
	for _, v := range volumes {
		inside := false
		for _, pts := range v.Boundaries {
			if math.PointInPolygon2LL(p, pts) {
				inside = !inside
			}
		}
		if inside {
			altRanges = append(altRanges, [2]int{v.LowerLimit, v.UpperLimit})
		}
	}

	// Sort altitude ranges and then merge ones that have 1000 foot separation
	sort.Slice(altRanges, func(i, j int) bool { return altRanges[i][0] < altRanges[j][0] })
	var mergedAlts [][2]int
	i := 0
	inside := false
	for i < len(altRanges) {
		low := altRanges[i][0]
		high := altRanges[i][1]

		for i+1 < len(altRanges) {
			if altRanges[i+1][0]-high <= 1000 {
				// merge
				high = altRanges[i+1][1]
				i++
			} else {
				break
			}
		}

		// 10 feet of slop for rounding error
		inside = inside || (int(alt)+10 >= low && int(alt)-10 <= high)

		mergedAlts = append(mergedAlts, [2]int{low, high})
		i++
	}

	return inside, mergedAlts
}
