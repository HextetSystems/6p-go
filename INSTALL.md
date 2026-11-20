# Installation and usage

## Install

Go 1.27.3 or newer is required.

```sh
go install github.com/HextetSystems/6p-go/cmd/6p@latest
```

To build from a checkout:

```sh
make build
# or directly:
go build ./cmd/6p
```

## Use

Input is read from standard input when no filename is given or when the
filename is `-`:

```sh
6p < file.txt
./some_task | grep something | 6p
6p file.txt
cat file.txt | 6p -
```

On success, `6p` writes only the absolute paste URL and a newline to standard
output. Diagnostics go to standard error and failures return a nonzero exit
status.

The default server is `https://paste.theos.cloud/`. Choose another server with
`SIXPASTER_SERVER` or `--server`; the flag takes precedence:

```sh
SIXPASTER_SERVER=https://paste.example.com/ 6p < file.txt
6p --server https://example.com/tools/paste/ file.txt
```

Pastes expire after seven days (`604800` seconds) by default. `--ttl` accepts
integer seconds or a Go duration that resolves to whole seconds:

```sh
6p --ttl 3600 < file.txt
6p --ttl 24h file.txt
6p --ttl 168h file.txt
```

The server decides which TTL range and content types are allowed. Text and
supported image data are passed through unchanged.

The original service limit is 307,200 bytes. The client prints a warning to
standard error when input exceeds that size but continues the upload, allowing
it to work with compatible servers configured for a larger limit.

Use `--truncate` to hard-limit UTF-8 text to the default 307,200-byte limit, or
use `--truncate=BYTES` for another limit:

```sh
some_noisy_command | 6p --truncate
6p --truncate=100000 large-log.txt
```

When truncation is needed, the client reserves space within the limit and ends
the paste with a blank line followed by `[Content truncated]`. No truncation
warning is printed because supplying the option acknowledges that truncation
may occur. Truncation accepts UTF-8 text only; binary files, including images,
can still be uploaded without this option. Custom limits must be at least 22
bytes and must use the `=` form so a bare `--truncate` never consumes the
following filename.
