package daemon

import "strconv"

// count puts a number in front of a noun and makes the noun plural unless
// the number is one: "1 secret", "3 secrets", "0 events". The nouns liman
// counts all take an s; a word that does not would need its own case.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
