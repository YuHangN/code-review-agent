package session

import "fmt"

var users = map[string]string{}

func SaveUser(id, name string) {
	go func() {
		users[id] = name
	}()
}

func FindUser(id string) string {
	return users[id]
}

func PrintUser(name string) {
	fmt.Printf("user=%d\n", name)
}
