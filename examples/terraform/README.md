# Terraform deployment example (AWS)

Provisions a single EC2 host that runs the orchestrator for a team, so
previews live at `<sha>-<repo>.preview.example.com` instead of
`preview.localhost`.

Everything the server owns is on one encrypted EBS volume; the instance is
otherwise disposable. Replace it and the data volume reattaches with every
registered repo, artifact, and state dir intact.

## What it provisions

- EC2 instance (Amazon Linux 2023) running `lahmanja/local-preview` under
  systemd, restarted on failure and on reboot
- A dedicated gp3 data volume mounted at `/var/lib/local-preview`
  (`prevent_destroy` — it holds the mirror clones, artifacts, and state)
- A security group restricted to `allowed_ingress_cidrs`
- An instance profile with SSM Session Manager (so port 22 stays closed)
- An Elastic IP, plus optional Route 53 records
- An ALB with an ACM wildcard certificate terminating HTTPS, and optional
  OIDC login in front of it (`enable_tls`, on by default)

## Usage

```hcl
module "local_preview" {
  source = "github.com/jmelahman/local-preview//examples/terraform?ref=v0.3.0"

  preview_domain        = "preview.example.com"
  allowed_ingress_cidrs = ["10.0.0.0/8"] # VPN range — see below
  route53_zone_id       = "Z123456789ABCDEFGHIJK"
  instance_type         = "t3.large"
}
```

```sh
terraform init
terraform apply
```

Then register a repo against the dashboard the module printed:

```sh
preview --server https://preview.example.com repo add my-app https://github.com/me/my-app
```

## There is no authentication

The server ships none. Anyone who can reach the dashboard can register a
repo and make this host run that repo's build commands — reaching it is
equivalent to shell access. The module therefore requires
`allowed_ingress_cidrs` and rejects `0.0.0.0/0`.

For a wider audience, authenticate in front of it — set `oidc` below, or use
Tailscale or Cloudflare Access — and keep the security group closed.

## DNS is two records, whatever the repo count

Previews resolve as `<sha>-<repo>.<preview_domain>`, which is a single label
below the base domain, so one wildcard covers every repo:

| Record | Serves |
| --- | --- |
| `*.<preview_domain>` | every preview |
| `<preview_domain>` | the dashboard (every Host that isn't `*.<preview_domain>`) |

Registering a repo needs no DNS change. Set `route53_zone_id` and the module
creates both — aliases to the load balancer with `enable_tls`, A records to
the Elastic IP without. Leave it unset and create what `dns_records` lists.

## TLS

On by default. The server itself only speaks HTTP, so `enable_tls` puts an
ALB in front holding an ACM certificate for `<preview_domain>` and
`*.<preview_domain>` — one certificate for every preview, because the host is
a single label. Port 80 redirects to 443, and the instance's security group
stops accepting anything but the load balancer.

The certificate is DNS-validated, so this needs `route53_zone_id`. To manage
DNS elsewhere, set `enable_tls = false` and terminate TLS yourself; the module
then points the records at the instance's Elastic IP and serves plain HTTP.

## Authenticating at the load balancer

`allowed_ingress_cidrs` is the whole security boundary by default. To widen
access, set `oidc` and the ALB requires a login before any request reaches the
server:

```hcl
oidc = {
  issuer                 = "https://accounts.google.com"
  authorization_endpoint = "https://accounts.google.com/o/oauth2/v2/auth"
  token_endpoint         = "https://oauth2.googleapis.com/token"
  user_info_endpoint     = "https://openidconnect.googleapis.com/v1/userinfo"
  client_id              = "..."
  client_secret          = var.oidc_client_secret
}

# The CLI can't follow a login redirect.
oidc_bypass_cidrs = ["10.0.0.0/8"]
```

Anything that isn't a browser — the `preview` CLI, webhook senders — cannot
complete the redirect, so it has to come from `oidc_bypass_cidrs`. Those
ranges reach the server unauthenticated, so keep them as tight as the
allowlist you'd have used anyway.

The client secret lands in Terraform state; keep the state encrypted, or pass
it from a secrets manager rather than a `.tfvars` file.

## Build toolchains

The published image bundles no toolchains, so on its own it can only build
targets whose build commands need nothing beyond a shell. The service mounts
the host docker socket, so targets that declare an `image` in their manifest
build in that image on the host's daemon — that, rather than baking toolchains
into the host, is the way to build real projects here. Backend `run` commands
still execute inside the server's container unless the manifest sets
`run_image`.

## Webhook secret

Set `github_webhook_secret_ssm_parameter` to the name of a SecureString
parameter and the instance reads it at boot into a root-only env file. Passing
the secret as a Terraform variable instead would persist it in state.

```sh
aws ssm put-parameter --name /local-preview/webhook-secret \
  --type SecureString --value "$(openssl rand -hex 32)"
```

Note that GitHub's webhook senders must reach the server — with a closed
security group they can't, so webhook triggers suit a self-hosted forge or an
allowlisted proxy. Watched repos (polling, `--poll-interval`) need no inbound
access at all.

## Upgrades

Pin `image` to a release tag and bump it to upgrade. The tag is baked into
the systemd unit that user_data writes, so applying a new one rebuilds the
instance; the data volume is a separate resource and reattaches with every
repo, artifact, and state dir intact. Expect a few minutes of downtime while
the replacement boots — the Elastic IP means DNS doesn't change.

```sh
terraform apply -var 'image=lahmanja/local-preview:0.3.0'
```

Note that Docker Hub tags carry no `v` prefix, unlike the git tags.
