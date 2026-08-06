# Install

The server shells out to `git` to manage repos and runs your projects' own
build commands, so `git` (and whatever toolchains your targets build with —
e.g. `go`, `npm`) must be on the `PATH`.

## PyPI (recommended)

```sh
uv tool install local-preview
```

This installs the `preview` binary to `~/.local/bin`. Make sure that
directory is on your `PATH`.

## GitHub releases

Download a prebuilt archive for your platform from the
[releases page](https://github.com/jmelahman/local-preview/releases) and
place the `preview` binary on your `PATH`.

## Docker

```sh
docker run -d --name preview \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v preview-data:/data \
  lahmanja/local-preview:latest
```

::: warning
The published image bundles `git` but no build toolchains (`go`, `npm`, …),
so it can register repos and serve the dashboard but can only build targets
whose build commands need nothing beyond a shell. Run the binary on the host
for real use; a batteries-included image is planned.
:::

## From source

```sh
git clone https://github.com/jmelahman/local-preview
cd local-preview
npm --prefix web ci && npm --prefix web run build
go build -tags embed -o preview .
```
