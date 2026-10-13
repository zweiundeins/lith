package board

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// WhoOf is the seed of a session's avatar: the first four bytes of the
// session id's SHA-256, in hex. Pages show avatars to everyone, so the
// seed never gives the session away.
func WhoOf(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:4])
}

// validWho accepts what WhoOf makes.
func validWho(s string) bool {
	if len(s) != 8 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Avatar is a person on the board: a name and a pixel face, both made from
// the seed alone, so every page draws the same one.
type Avatar struct {
	Who  string
	Name string
	// Hue is the face's hue in oklch degrees.
	Hue int
	// Path draws the face on a 7×7 grid (a pixel of margin around 5×5),
	// mirrored left to right.
	Path string
}

var (
	adjectives = [16]string{"Amber", "Brisk", "Calm", "Deft", "Eager", "Fond", "Glad", "Hardy", "Jolly", "Keen", "Lucid", "Merry", "Nimble", "Plucky", "Quick", "Sunny"}
	stones     = [16]string{"Basalt", "Flint", "Granite", "Quartz", "Slate", "Marble", "Onyx", "Jasper", "Agate", "Shale", "Pumice", "Garnet", "Obsidian", "Mica", "Opal", "Gneiss"}
)

// AvatarOf draws the avatar of a seed from WhoOf. The seed's 32 bits pick
// the face (15 bits: three columns of five, the third mirrored onto the
// first two), the hue (4 bits) and the name (8 bits).
func AvatarOf(who string) Avatar {
	b, err := hex.DecodeString(who)
	if err != nil || len(b) != 4 {
		b = []byte{0, 0, 0, 0}
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	face := n & 0x7fff
	// A face needs enough pixels to read as one: sparse ones turn over.
	if ones(face) < 6 {
		face ^= 0x7fff
	}
	// The grid, then each row as runs of pixels: a path of a rectangle
	// per run is a third of one per pixel.
	var px [5][5]bool
	for col := 0; col < 3; col++ {
		for row := 0; row < 5; row++ {
			if face&(1<<(col*5+row)) != 0 {
				px[row][col], px[row][4-col] = true, true
			}
		}
	}
	var p strings.Builder
	for row := range px {
		for x := 0; x < 5; {
			if !px[row][x] {
				x++
				continue
			}
			w := 1
			for x+w < 5 && px[row][x+w] {
				w++
			}
			n := strconv.Itoa(w)
			p.WriteString("M" + strconv.Itoa(x+1) + " " + strconv.Itoa(row+1) + "h" + n + "v1h-" + n + "z")
			x += w
		}
	}
	return Avatar{
		Who:  who,
		Name: adjectives[n>>27&15] + " " + stones[n>>23&15],
		Hue:  int(n>>15&15) * 360 / 16,
		Path: p.String(),
	}
}

func ones(n uint32) int {
	c := 0
	for ; n != 0; n &= n - 1 {
		c++
	}
	return c
}
