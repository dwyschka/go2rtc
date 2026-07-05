//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("petkit-talktest only runs on the Petkit device (linux)")
}
