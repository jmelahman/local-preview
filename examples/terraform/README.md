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

## Usage

```hcl
module "local_preview" {
  source = "github.com/jmelahman/local-preview//examples/terraform?ref=v0.1.0"

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
preview --server http://preview.example.com repo add my-app https://github.com/me/my-app
```

## There is no authentication

The server ships none. Anyone who can reach the dashboard can register a
repo and make this host run that repo's build commands — reaching it is
equivalent to shell access. The module therefore requires
`allowed_ingress_cidrs` and rejects `0.0.0.0/0`.

For a wider audience, put an authenticating proxy (an ALB with OIDC, Tailscale,
Cloudflare Access) in front and keep the security group closed to it.

## DNS is two records, whatever the repo count

Previews resolve as `<sha>-<repo>.<preview_domain>`, which is a single label
below the base domain, so one wildcard covers every repo:

| Record | Serves |
| --- | --- |
| `A *.<preview_domain>` | every preview |
| `A <preview_domain>` | the dashboard (every Host that isn't `*.<preview_domain>`) |

Registering a repo needs no DNS change. Set `route53_zone_id` and the module
creates both; leave it unset and create what the `dns_records` output lists.

## Plain HTTP

The example serves HTTP on port 80 and stops there. Nothing structural blocks
TLS — the single-label host means one `*.<preview_domain>` certificate covers
every preview — but terminating it needs something in front of the instance
(an ALB with an ACM cert, or a reverse proxy on the box doing DNS-01), which
this example deliberately leaves out.

Until then, and given the server has no authentication, this is a deployment
for a trusted network rather than the public internet.

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

`image` defaults to `:latest`, which is only re-pulled when the service
restarts. Pin a release tag and change it to upgrade deliberately:

```sh
aws ssm start-session --target <instance-id>
sudo systemctl restart local-preview
```
