# Runbook: move the CA key off vm2 to the operator machine

Design: `key-custody-hsm.md` 5.1 condition 2. The CA key must not live on a node that runs workloads (vm2 runs other tenants' containers and `hsm-gateway`).
After this, only the operator machine can issue certificates; nodes get leaves pushed (`make push-certs`) and never sign anything. **You run these; nothing here was run on vm2/vm3.**

## Choose

- **A. Move the existing CA** (leaves stay valid, no mass re-issue). Fine only if you trust that nobody else had root on vm2 so far.
- **B. New CA** (recommended if vm2 ever hosted other tenants: a copy of the old key cannot be ruled out). Every leaf is re-issued, every pin changes.

## A. Move

On the operator machine (a laptop you control, disk encrypted):

    mkdir -p ~/.digiwal-ca && chmod 700 ~/.digiwal-ca
    scp -p vm2:'.digiwal-ca/ca-key.pem' vm2:'.digiwal-ca/ca-cert.pem' ~/.digiwal-ca/
    ssh vm2 'sha256sum ~/.digiwal-ca/ca-key.pem'; sha256sum ~/.digiwal-ca/ca-key.pem     # the two must match
    chmod 400 ~/.digiwal-ca/ca-key.pem                                                   # keep one offline copy (encrypted) too

## B. New CA

    infra/pki/gen-ca.sh        # on the operator machine; refuses to overwrite an existing CA

## Then (both)

1. Operator: `make -C dev push-certs` issues every missing leaf and pushes each node its own (key 0400, directory 0500, owned by the service uid), plus the pinned callers' public certificates to vm2.
   (A with leaves already on the nodes: they are re-issued on first push because the operator has none yet; new keys, same CA. That is intended.)
2. vm2: `make -C dev hsm-setup` (re-pins `callers.yaml`), then `make -C dev up`. vm3: `make -C dev vault-files`, then `make -C dev up` (recreates services with the new pins).
3. Only after the stack runs on the new leaves, remove the old CA from vm2: `ssh vm2 'shred -u ~/.digiwal-ca/ca-key.pem; rm -rf ~/.digiwal-ca'`. (shred is best effort on SSD, snapshots and copy-on-write disks: this is why B exists.)
4. `make -C dev verify` on **both** nodes: "no CA key on this node" must PASS (it searches `~/.digiwal-ca` and `/root`, `/home`, `/etc/digiwal`, `/opt`, `/srv`). B only: a mixed CA is never trusted, so a leaf from the old CA left anywhere fails the mTLS handshake loudly.

Rotation later (leaf expiry is 2 years): on the operator, delete `infra/pki/out/<svc>` (`chmod -R u+w` first), `make -C dev push-certs`, then step 2 for the pins.
