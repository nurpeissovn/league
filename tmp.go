package main
import (
    "fmt"
    pq "github.com/lib/pq"
)
func main() {
    u := "postgresql://user:pass@host:5432/db"
    s, err := pq.ParseURL(u)
    fmt.Println("err:", err)
    fmt.Println("s:", s)
}
