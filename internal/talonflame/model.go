package talonflame

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const javaMultiplier uint64 = 0x5DEECE66D
const javaMask uint64 = (1 << 48) - 1

func SpeciesRoll(encounterID uint64) uint32 {
	state := (encounterID ^ javaMultiplier) & javaMask
	state = (state*javaMultiplier + 11) & javaMask
	return uint32(state >> 24)
}

type Pair struct {
	Pokemon int `json:"pokemon_id"`
	Form    int `json:"pokemon_form"`
}

func (p Pair) String() string { return fmt.Sprintf("%d/%d", p.Pokemon, p.Form) }

// ID decodes string/numeric JSON without ever passing through float64.
type ID string

func (id *ID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*id = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*id = ID(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*id = ID(number.String())
	return nil
}

type Webhook struct {
	Type    string  `json:"type"`
	Message Pokemon `json:"message"`
}

type Pokemon struct {
	EncounterID  ID     `json:"encounter_id"`
	SpawnpointID string `json:"spawnpoint_id"`
	PokemonID    int    `json:"pokemon_id"`
	Form         *int   `json:"form"`
	SeenType     string `json:"seen_type"`
	IsEvent      int    `json:"is_event"`
	IsDitto      bool   `json:"is_ditto"`
	Disappears   int64  `json:"disappear_time"`
	Modified     *int64 `json:"last_modified_time"`
}

type Observation struct {
	Encounter  uint64
	Spawnpoint uint64
	Nest       int64
	Pair       Pair
	Roll       uint32
	At         int64
	Hour       int64
}

type evidenceKey struct {
	Hour                  int64
	Encounter, Spawnpoint uint64
}

func (o Observation) key() evidenceKey { return evidenceKey{o.Hour, o.Encounter, o.Spawnpoint} }

func parsePokemon(p Pokemon, now time.Time) (Observation, bool, error) {
	var result Observation
	if (p.SeenType != "wild" && p.SeenType != "encounter") || p.IsEvent != 0 || p.IsDitto || p.PokemonID == 132 {
		return result, false, nil
	}
	if p.PokemonID <= 0 || (p.Disappears > 0 && p.Disappears <= now.Unix()) {
		return result, false, nil
	}
	if p.Form == nil || *p.Form < 0 {
		// Unknown form is not the same as form 0; a later update can complete it.
		return result, false, nil
	}
	text := string(p.EncounterID)
	base := 10
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") {
		base, text = 16, text[2:]
	}
	encounter, err := strconv.ParseUint(text, base, 64)
	if err != nil || encounter == 0 {
		return result, false, fmt.Errorf("invalid encounter ID")
	}
	spawnpoint, err := strconv.ParseUint(p.SpawnpointID, 16, 64)
	if err != nil || spawnpoint == 0 {
		return result, false, nil
	}
	roll := SpeciesRoll(encounter)
	if roll >= 1<<22 {
		return result, false, nil
	}
	at := now.Unix()
	if p.Modified != nil && *p.Modified > 0 {
		at = *p.Modified
		if at > 100000000000 {
			at /= 1000
		}
		if at > now.Unix()+60 {
			return result, false, fmt.Errorf("observation timestamp is in the future")
		}
	}
	return Observation{Encounter: encounter, Spawnpoint: spawnpoint,
		Pair: Pair{p.PokemonID, *p.Form}, Roll: roll, At: at}, true, nil
}

func hourBoundary(at time.Time, zone *time.Location) int64 {
	local := at.In(zone)
	return at.Unix() - int64(local.Minute()*60+local.Second())
}

type Group struct {
	ID          int64  `json:"nest_id"`
	Pair        Pair   `json:"species"`
	Confirmed   bool   `json:"confirmed"`
	Conflicting bool   `json:"conflicting"`
	LastChanged *int64 `json:"last_changed"`
	Timezone    string `json:"timezone,omitempty"`
	zone        *time.Location
}

type pointState struct {
	Nest   int64
	Status string
}
