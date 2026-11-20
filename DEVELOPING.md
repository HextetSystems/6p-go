# Development

Run `make help` to list available tasks. Common tasks are:

```sh
make build
make test
make check
make clean
```

## Go package

The `paster` package streams an `io.Reader` to a compatible service:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/HextetSystems/6p-go/paster"
)

func main() {
	client, err := paster.New("https://paste.theos.cloud/", nil)
	if err != nil {
		panic(err)
	}

	u, err := client.Create(context.Background(), os.Stdin, 7*24*time.Hour)
	if err != nil {
		panic(err)
	}
	fmt.Println(u)
}
```

`Create` sends a multipart `POST` with `content` and integer-seconds `ttl`
fields. It accepts the redirect response used by both 6paster implementations
without fetching the newly created paste. If a service returns an HTML page
instead, the client reports the page's marked error message (when present)
rather than printing the full document.
