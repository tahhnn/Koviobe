// Package avatar owns the emoji avatar a player picks when joining a room.
//
// An avatar is stored as its Twemoji code (the SVG file name the frontend
// serves from /avatars/<code>.svg), never as the emoji character: a code is
// plain ASCII, has one spelling, and cannot smuggle text or a variation
// selector into a column that is echoed on a projector.
//
// The set is closed on purpose. A free-form emoji field would let a player
// put anything on the host's screen in front of the room; this list is the
// only thing that can appear there. The frontend (lib/avatar.ts) carries the
// same list; a code it does not know simply renders nothing.
package avatar

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// Codes is the full allowed set, in picker order.
var Codes = []string{
	"1f98a", // fox
	"1f43c", // panda
	"1f42f", // tiger
	"1f438", // frog
	"1f428", // koala
	"1f981", // lion
	"1f419", // octopus
	"1f984", // unicorn
	"1f427", // penguin
	"1f435", // monkey
	"1f41d", // bee
	"1f433", // whale
	"1f989", // owl
	"1f430", // rabbit
	"1f436", // dog
	"1f431", // cat
	"1f43b", // bear
	"1f437", // pig
	"1f42e", // cow
	"1f414", // chicken
	"1f996", // t-rex
	"1f422", // turtle
	"1f98b", // butterfly
	"1f42c", // dolphin
	"1f980", // crab
	"1f41e", // lady beetle
	"1f992", // giraffe
	"1f994", // hedgehog
	"1f432", // dragon face
	"1f47b", // ghost
	"1f916", // robot
	"1f47d", // alien
}

var allowed = func() map[string]bool {
	m := make(map[string]bool, len(Codes))
	for _, c := range Codes {
		m[c] = true
	}
	return m
}()

// Valid reports whether code is one of the allowed avatars.
func Valid(code string) bool {
	return allowed[code]
}

// Random returns an allowed avatar chosen uniformly at random.
func Random() string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(Codes))))
	if err != nil {
		return Codes[0]
	}
	return Codes[n.Int64()]
}

// Resolve is the join rule: a valid pick is kept, anything else — missing,
// unknown, from an older frontend — gets a random avatar. It never fails:
// an avatar is decoration, and refusing a player over it would be a bug.
func Resolve(requested string) string {
	code := strings.ToLower(strings.TrimSpace(requested))
	if Valid(code) {
		return code
	}
	return Random()
}
