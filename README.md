# capacity

A small, clean **cloud capacity-provider interface** for Go, plus a complete **AWS Spot reference
implementation** and a **conformance suite**. It's the extension point for provisioning ephemeral
compute (GPU workers) across clouds: implement one interface, pass the conformance suite, and a
scheduler can shop and launch on your source of capacity.

## The interface

```go
type Provider interface {
    Name() string
    Quote(ctx, Requirement) ([]Offer, error)                      // live offers that satisfy the requirement
    Launch(ctx, id string, o Offer, spec WorkerSpec) (WorkerHandle, error) // provision a worker onto an offer
    Terminate(ctx, WorkerHandle) error                            // tear it down
    ListOwned(ctx) ([]OwnedInstance, error)                       // every instance we own — the anti-orphan safety net
}
```

That's the whole contract. `Quote` surfaces priced options; the caller scores them (this package is
deliberately *not* opinionated about which offer to pick). `ListOwned` is the safety net: cross-check
it against your own registry and terminate anything untracked, so no instance can bill forever after
a state loss.

## What's in the box

- **`capacity`** — the `Provider` interface, domain types (`Offer`, `Requirement`, `WorkerSpec`,
  `WorkerHandle`), a `fake` provider for tests, and composable wrappers: `gated` (respect an
  operator on/off switch), `multiregion`, and `multicloud` (fan `Quote` across several providers).
- **`capacity/aws`** — a working **AWS EC2 Spot** provider: quotes spot offers, launches tagged
  instances, terminates, and lists owned instances by tag.
- **`InstanceAuditor`** — a small **optional** capability, separate from `Provider`: list *every*
  running instance in the account, owned or not. `ListOwned` only sees what you tagged, so it can
  never surface a leak from outside the system (an untagged image builder, a box someone forgot);
  an account-wide sweep can. Implemented by the AWS provider, and fanned across every region/cloud
  by `multiregion`/`multicloud` — providers that don't implement it are skipped rather than
  failing the sweep.
- **Conformance suite** — `capacity.RunConformance(t, newProvider, …)` exercises any implementation
  against the contract, so "I added a provider" means "I added a *conformant* provider."

## Install

```
go get github.com/getotium/capacity
```

## Adding a provider

Implement `Provider` for your cloud (GCP, Azure, OCI, RunPod, Vast.ai, bare-metal, a homelab…), then
in a test:

```go
func TestMyProvider(t *testing.T) {
    capacity.RunConformance(t, func(t *testing.T) capacity.Provider { return newMyProvider(t) }, model, instanceType)
}
```

## Provenance

Extracted from [Otium](https://getotium.ai), where it's the boundary between the scheduler and the
compute it runs on. The interface is open precisely so anyone can contribute capacity; the tuned
*placement/pricing strategy* that decides which offer to take stays in Otium.

## License

[Apache-2.0](LICENSE).
