# QuorvexFusion

A self-hosted compute cluster in Go: machines on your LAN or anywhere on the
internet join a shared pool of CPU, RAM, and GPUs, and jobs submitted to the
pool are scheduled onto them automatically.

- **One binary** (`cluster`) for the controller, the workers and the CLI.
- **Workers only dial out** to the controller (gRPC over mutual TLS), so they
  work behind NAT, inside containers and on rented GPU boxes.
- **No external services**: state lives in an embedded SQLite database.
- Linux only (amd64 and arm64).

> **Status:** under active development. Working today: joining nodes,
> hardware/metrics reporting, scheduling, shell jobs, array jobs, retries,
> timeouts, live logs, cgroup limits, the installer, LAN discovery, the
> web UI, file transfer, remote and ephemeral nodes, Let's Encrypt, Docker
> and GPU containers. Coming next: release binaries and packaging.

## Quick start

### 1. Install the controller

On the machine that will coordinate the cluster:

```sh
git clone https://github.com/NTFespolion307/QuorvexFusion.git
cd QuorvexFusion
sudo ./install.sh controller
```

The installer builds the binary (downloading Go temporarily if needed), asks
a few questions (ports, admin password, whether this machine should also run
a worker), and starts a `cluster-controller` systemd service. It ends by
printing the web UI address and a **join code** such as
`7KQ2-MX4P-9TRA-BH3W-C8NE`.

### 2. Join workers

On every other machine:

```sh
git clone https://github.com/NTFespolion307/QuorvexFusion.git
cd QuorvexFusion
sudo ./install.sh worker
```

It finds the controller on your LAN and asks for the join code. That's all:
the code also verifies that the controller is really yours (it contains a
pin of the controller's certificate), so there's no fingerprint to compare.
Codes are case-insensitive and the dashes are optional.

If the controller is on another network (VPN, cloud, internet), give its
address; port 7443 is assumed:

```sh
sudo ./install.sh worker --controller 203.0.113.7 --code 7KQ2-MX4P-9TRA-BH3W-C8NE
```

Need another code? On the controller: `cluster token create`
(`--max-uses 1`, `--expires 1h`, or `--manual-approve` to make new nodes
wait for `cluster nodes approve <node>`).

### 3. Run jobs

The installer configures the CLI on the controller for root and for the
user who ran `sudo`:

```sh
cluster status                                    # pool size and usage
cluster nodes                                     # machines in the pool
cluster submit -f -- uname -a                     # run one command, stream its output
cluster submit --cpus 4 --memory 8G -- ./render.sh 12
cluster submit --wait --array 1-500 -- 'python3 sim.py --seed {i}'
cluster submit --retries 2 --timeout 2h --gpus 1 -- python3 train.py
cluster jobs                                      # recent jobs
cluster jobs show <job>                           # tasks, nodes, exit codes
cluster logs -f <job>.<index>                     # live output of one task
cluster cancel <job>
```

Each task runs with `/bin/sh -c` on whichever node has room, in its own
working directory, as the unprivileged `cluster` user, inside a cgroup
limiting it to the CPUs and memory it asked for. `{i}` is replaced by the
array index. Tasks see `CLUSTER_TASK_ID`, `CLUSTER_ARRAY_INDEX`,
`CLUSTER_ATTEMPT` and, for GPU tasks, `CUDA_VISIBLE_DEVICES` limited to their
own GPUs.

To use the CLI from another computer:
`cluster login --controller CONTROLLER_IP:8443` (asks for the admin password).

## Files: inputs, outputs and scripts

Remote machines don't share your disk, so jobs carry their files with them:

```sh
# Inputs: files or folders from this computer, placed in every task's working directory
cluster submit --input scene.blend --input textures/ --output 'frames/*' --array 1-250:10 -- \
  'blender -b scene.blend -o //frames/f_#### -s {i} -e $(( {i} + 9 )) -a'

# A script is uploaded and run; arguments after -- are passed to it
cluster submit --script train.py --input data/ --output 'model/**' --gpus 1 -- --epochs 10

# Collect the outputs (array jobs get one folder per task index)
cluster outputs <job>              # into ./<job>/
cluster outputs <job> --list
```

- `--input SRC[:DEST]` places a file or folder at `DEST` (default: its name).
  Each file is uploaded once (identical files are stored once), verified by
  SHA-256, and cached on every node that downloads it, so running the same
  job again downloads nothing. Inputs are read-only in the task; copy them if
  the task must modify them.
- `--output GLOB` collects matching files after each task (`*` within a
  folder, `**` across folders; a folder name collects everything in it).
  Outputs of the attempt that completed the task are kept, for finished and
  timed-out tasks.
- All transfers resume after interruptions. Progress shows in the CLI and on
  the task page.
- **File library** (web UI *Files* page, or `cluster files`): upload files
  and folders to the controller once, from wherever you are, then use them
  in any number of jobs without uploading again:
  ```sh
  cluster files upload scene.blend textures/ --to city   # resumable; also from your PC after `cluster login`
  cluster files ls
  cluster submit --file city --output 'frames/*' -- 'blender -b city/scene.blend ...'
  ```
  In the web UI, pick them under *Inputs from your files* on the New job
  form, or press **Run** next to a script on the *Files* page to open a job
  that runs it (`python3 ...` for `.py`, `bash ...` for `.sh`, ...). Library
  files are read-only in the task, so scripts are started through their
  interpreter. Browser uploads go in 8 MB chunks and resume after a dropped
  connection (pick the same file again if the page was closed).
- **Shared storage**: if nodes mount the same storage (NFS, a NAS), start
  their workers with `--shared-storage /mnt/shared` (or set
  `CLUSTER_SHARED_STORAGE` in `/etc/cluster/worker.env`) and use
  `--shared-input datasets/big:data` to link a path from it without any
  transfer. Such jobs only run on nodes with shared storage.
- Workers keep at most 20 GiB of cached inputs by default
  (`--cache-max 100G` to change).

## Docker containers and GPUs

Run a task inside a container image instead of on the node directly, so
nodes don't need your software installed:

```sh
cluster submit --image python:3.12-slim --input data/ --output 'out/*' -- python3 analyse.py
cluster submit --image blender/blender:4.2 --gpus 1 --array 1-250:10 --input scene.blend --output 'frames/*' -- \
  'blender -b scene.blend -o //frames/f_#### -s {i} -e $(( {i} + 9 )) -a'
cluster submit -f --image nvidia/cuda:12.4.1-base-ubuntu22.04 --gpus 1 -- nvidia-smi
```

- The task's working directory (with its input files) is mounted at
  `/work`, which is also the current directory; outputs are collected from
  it as usual. With a command, it runs with `/bin/sh -c` inside the
  container; without one, the image's default command runs.
- `--cpus`, `--memory` and `--gpus` become Docker limits. A task asking for
  GPUs gets exactly its assigned GPUs (`--gpus device=...`), numbered from 0
  inside the container.
- Container tasks only go to nodes where Docker works, and GPU containers
  only to nodes with the [NVIDIA container toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
  (the node cards show "no docker" / "gpu containers").
- Images are pulled on first use (the pull appears in the task log and does
  not count against `--timeout`). For private registries, run `docker login`
  on the nodes.
- Containers run as the unprivileged task user, so files in `/work` stay
  owned by it; images that must run as root inside may need adjusting.
- Plain (non-container) GPU tasks work too, on any node with NVIDIA drivers:
  `cluster submit --gpus 1 -- python3 train.py` sets `CUDA_VISIBLE_DEVICES`
  to the task's GPUs.

## Web UI

Open `https://CONTROLLER_IP:8443` and sign in with the admin password.

- **Dashboard**: pool size and usage, nodes and tasks, CPU/GPU charts by location.
- **Nodes**: live usage per machine; approve, drain, revoke and label nodes.
  Click a node for its hardware and live charts (per-core CPU, memory, network, GPUs).
- **Jobs**: submit jobs, follow progress, read live task output, cancel.
- **Join tokens**: create join codes for new machines.
- **Settings**: admin password, API tokens, the CA certificate.

The browser warns about the certificate because it is signed by the cluster's
own CA. Accept it once, or remove the warning for good by importing the CA
certificate (Settings → Download CA certificate) into your browser or OS
trust store.

## Managing the installation

| Task | Command |
|---|---|
| Upgrade after `git pull` | `sudo ./install.sh upgrade` |
| Remove, keep data | `sudo ./install.sh uninstall` |
| Remove everything | `sudo ./install.sh uninstall --purge` |
| Controller logs | `journalctl -u cluster-controller -f` |
| Worker logs | `journalctl -u cluster-worker -f` |
| Worker settings (location, labels) | edit `/etc/cluster/worker.env`, then `sudo systemctl restart cluster-worker` |
| Change admin password | `sudo -u clusterctl cluster controller passwd --data-dir /var/lib/cluster` |
| Change address, domain, ephemeral timeout | `sudo cluster controller set --help` |
| Change a node's location, labels, local/remote | web UI *Edit*, or `cluster nodes set NODE --help` |
| See what a worker detects | `cluster worker probe` |

All installer options: `./install.sh controller --help`, `./install.sh worker --help`.

## Joining machines from anywhere

Workers only make outgoing connections, so worker machines never need port
forwarding, a public IP or open ports. Only the controller must be
reachable (see [Networking](#networking)).

### A machine on your LAN

```sh
git clone https://github.com/NTFespolion307/QuorvexFusion.git && cd QuorvexFusion
sudo ./install.sh worker          # finds the controller, asks for the join code
```

### A rented GPU box (vast.ai and similar)

Rented instances are containers without systemd, and they come and go, so
mark them **ephemeral**: when one stays offline longer than the ephemeral
timeout (1 hour by default) it is removed from the cluster automatically,
and its tasks are requeued as soon as it disappears.

Create a code for them on the controller (or in the web UI, *Join tokens*):

```sh
cluster token create --location vastai --ephemeral --expires 720h --description "vast.ai"
```

Then use the printed one-liner as the instance's **on-start script**:

```sh
curl -fsSL https://raw.githubusercontent.com/NTFespolion307/QuorvexFusion/main/bootstrap.sh \
  | sh -s -- --controller controller1.example.com --code 7KQ2-MX4P-9TRA-BH3W-C8NE
```

It installs the `cluster` binary, joins with the code and keeps the worker
running in the background (log: `/var/log/cluster-worker.log`); restarting
the instance reuses the same identity. GPUs are detected with `nvidia-smi`,
so pick an image with the software your jobs need (CUDA, PyTorch,
Blender, ...) and submit with `--gpus 1`. Notes:

- Inside such containers Docker is usually unavailable, so run tasks as
  plain commands (they are, by default).
- CPU and memory limits are not enforced without systemd; the scheduler
  still never places more work than the machine's capacity.
- A `--require location=vastai` or `--no-ephemeral` / `--no-remote` on a
  job controls whether it may use rented machines.

### A cloud VM

On a VM with systemd, the same one-liner installs a proper `cluster-worker`
service (it needs root, e.g. in cloud-init's `runcmd`):

```sh
curl -fsSL https://raw.githubusercontent.com/NTFespolion307/QuorvexFusion/main/bootstrap.sh \
  | sudo sh -s -- --controller controller1.example.com --code ... --location gcp-us-central1
```

Add `--ephemeral` for preemptible/spot VMs.

### Any other supervisor

The worker is a single foreground process; run it under whatever manages
your processes (Docker, supervisord, runit, a Kubernetes pod):

```sh
cluster worker --controller CONTROLLER --code 7KQ2-MX4P-9TRA-BH3W-C8NE \
  --data-dir /var/lib/cluster-worker --location office
```

The code is only needed for the first run; the node's identity is saved in
`--data-dir`. Keep that directory on persistent storage. Exit code 3 means
the node was revoked: don't restart it then.

## Networking

Two ports, both TLS with the cluster's own certificate authority:

| Port | Used by | Needs to be reachable from |
|---|---|---|
| **7443** | workers | every worker |
| **8443** | web UI, CLI, API | wherever you browse/administer from |

Workers recognise the controller by its certificate, not by its name or
address, so they work whether they reach it by IP, LAN name, domain or
VPN name.

### Setup 1: controller exposed to the internet (port forwarding)

Use this when remote machines (rented GPUs, cloud VMs, friends' PCs) should
join over the internet.

1. Give the controller machine a fixed LAN address (DHCP reservation on the
   router), e.g. `192.168.0.120`.
2. On the router, forward TCP **7443** to `192.168.0.120:7443`. Forward
   **8443** too if you want the web UI from outside.
3. Optional but recommended: a domain name. Create a DNS `A` record such as
   `controller1.example.com` pointing at your public IP (use a dynamic-DNS
   service if that IP changes), then tell the controller:
   ```sh
   sudo cluster controller set --public-addr controller1.example.com
   ```
   Join commands and codes now use that name.
4. Inside your LAN, the domain resolves to your public IP, which many
   routers don't loop back ("NAT hairpinning"). Either add a local DNS
   entry (router or Pi-hole) mapping the domain to `192.168.0.120`, or keep
   using the LAN IP for local machines.

**A real certificate for the web UI (Let's Encrypt).** Browsers warn about
the cluster's own certificate. With a domain you can get a trusted one:

```sh
sudo cluster controller set --domain controller1.example.com --acme-email you@example.com
```

Let's Encrypt checks the domain on port **443**, so forward external port
443 to the controller's UI port (8443), or make the UI listen on 443
directly (`--http-listen :443`) and forward 443 to 443. If you'd rather use
port 80 for the check, add `--acme-http-listen :80` and forward port 80. The
certificate is renewed automatically. Access by IP keeps working (with the
cluster's own certificate), and so does everything else if Let's Encrypt
is ever unreachable.

**What is exposed:** the worker port accepts only nodes holding a
certificate the controller issued, plus join attempts, which need a valid
code and are rate-limited. The web port serves only the login page without
a session; logins are rate-limited. Use a strong admin password.

### Setup 2: everything on a VPN (Tailscale, Headscale, WireGuard)

Use this when you don't want anything exposed to the internet. Put the
controller and all workers on the same overlay network, then make the
controller listen only on its VPN address:

```sh
sudo ./install.sh controller --listen-ip 100.101.102.103   # the controller's Tailscale IP
sudo ./install.sh worker --controller 100.101.102.103 --code ...   # or its Tailscale name
```

Nothing needs port forwarding, and the controller is invisible outside the
VPN. LAN discovery (mDNS) doesn't cross VPNs, so give workers the address.

### Local and remote nodes

Each node is classified by the address it connects from: LAN, VPN
(including Tailscale's 100.64.0.0/10) and loopback addresses are **local**;
anything else is **remote**. The scheduler prefers local nodes when several
fit, and a job submitted with `--no-remote` never leaves your network.
Override the classification per node in the web UI (*Edit*) or with
`cluster nodes set NODE --network local|remote|auto`.

## Development

```sh
./dev.sh up 2                      # build and start a controller + 2 workers in ./dev-data
./dev.sh cli submit -f -- echo hi  # use the CLI against it
./dev.sh down                      # stop (./dev.sh clean also deletes the data)
make test                          # unit and integration tests
make cross                         # linux/amd64 and linux/arm64 binaries in dist/
```

The code is organised by responsibility under `internal/`:
`controller` (node sessions, task lifecycle), `scheduler` (pure placement
logic), `worker` and `runner` (agent and task execution), `store` (SQLite),
`api` (REST), `pki` (certificates), `hw` (hardware probes), `discovery` (mDNS).
The wire protocol is in `internal/clusterpb/cluster.proto`.
