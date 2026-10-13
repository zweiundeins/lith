// Command timetick shows that a time.Time with nanoseconds below the 100 ns
// tick is refused, while a time.Duration is truncated.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"stardust"
)

func main() {
	token, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	if err := stardust.Activate(strings.TrimSpace(string(token))); err != nil {
		panic(err)
	}
	at := time.Date(2026, 10, 13, 6, 0, 0, 123456789, time.UTC)
	_, err = stardust.FromValue(map[string]any{"at": at})
	fmt.Println("time.Time with 123456789 ns:", err)
	_, err = stardust.FromValue(map[string]any{"after": 1234567891 * time.Nanosecond})
	fmt.Println("time.Duration with sub-tick ns:", err)
	_, err = stardust.FromValue(map[string]any{"at": at.Truncate(100 * time.Nanosecond)})
	fmt.Println("truncated to 100 ns:", err)
}
