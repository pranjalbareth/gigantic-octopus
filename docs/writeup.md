# My TTL timer lied: rebuilding an environment platform as a level-triggered controller

*Draft. Replace the bracketed notes with your own numbers and words before publishing.*

## The bug

Runway v0 was a Node.js app that created short-lived environments and
destroyed them after a TTL. Destroying was one line:

```js
setTimeout(() => destroy(env), env.ttlMs)
```

It worked until the process restarted. Every pending timer lived in memory, so
a deploy, a crash or a laptop going to sleep silently cancelled every
scheduled destroy. Expired environments stayed up, and nothing in the logs
said so, because the thing that should have logged no longer existed.

The fix was to store `expires_at` and, on boot, destroy whatever was already
past it. That fix is small, and it changes what the program is. Before, it
reacted to an event: "the timer fired". After, it compared two facts: "what
time is it" and "when does this expire". [One sentence on how you found it.]

## Edge-triggered and level-triggered

A doorbell is edge-triggered: miss the ring and you never know someone came.
A thermostat is level-triggered: it does not care when the room got cold,
only that it is cold now.

`setTimeout` is a doorbell. Kubernetes controllers are thermostats. A
controller's reconcile function is not told what happened; it is handed a
name and has to look at the world, decide what should be true, and make it
so. If it misses ten notifications and gets the eleventh, the result is the
same. If it crashes, the next process starts by listing everything and
reconciling each object once.

So I rebuilt Runway as a controller.

## Expiry with no timer

An `Environment` is a custom resource:

```yaml
apiVersion: runway.pranjalbareth.github.io/v1alpha1
kind: Environment
metadata:
  name: pr-1423
spec:
  template: web-small
  ttl: 8h
```

Expiry is defined in one function:

```go
func ExpiresAt(env *Environment) time.Time {
    return env.CreationTimestamp.Add(env.Spec.TTL.Duration)
}
```

Both inputs are stored in the API server. The reconcile loop is eight steps;
the two that matter here are:

- If now is at or past `ExpiresAt`, delete the Environment.
- Otherwise, return `RequeueAfter: ExpiresAt - now`.

`RequeueAfter` looks like the old timer, and it is one: it lives in memory
and dies with the process. The difference is that nothing depends on it. It
is a wake-up call. If it is lost, the next reconcile for any reason reaches
the same comparison and gets the same answer.

## Proving it

A claim like "survives restarts" is cheap to make, so the project has a test
that tries to break it on a real cluster:

1. Create an Environment with a two-minute TTL and wait until it is Ready.
2. Scale the controller to zero.
3. Wait until thirty seconds past expiry.
4. Check the Environment is still there. Nothing else cleaned it up.
5. Scale the controller back to one.
6. Assert the Environment and its namespace are gone.

[Paste the passing run. Add the value of `runway_expiry_lag_seconds` you saw:
it is the length of the outage, which is the right answer.]

The same property is tested faster with a fake clock: build a reconciler,
let it provision, throw it away, build a new one whose clock reads an hour
later, and call reconcile once.

## Three things the rewrite forced me to get right

**Extension is desired state.** My first design had an annotation,
`extend: 4h`, that the controller would apply and then remove. That is a
doorbell again: a one-shot message hidden in the object. Now you raise
`spec.ttl`, and the controller notices that expiry moved.

**Deletion needed a finalizer after all.** Every object the controller
creates is owned by the Environment, so garbage collection would remove them.
But it does so in the background, after the Environment is already gone.
The finalizer deletes the namespace and waits for it, so when a delete
returns, the name is free and the per-user limit is honest.

**The clock is an input.** The reconciler never calls `time.Now()`. It reads
an injected clock, which is why the tests step time instead of sleeping, and
why "exactly at expiry" is a test case and not a race.

## What it does not do

[Keep this section. It is what makes the rest believable.]

- The per-owner limit is a count followed by a create. Two simultaneous
  requests can both pass.
- `creationTimestamp` comes from the API server's clock and "now" from the
  controller's. Skew between them moves expiry by the same amount.
- For real workloads, use vcluster, Okteto or Argo CD ApplicationSets. Runway
  is about 2,250 lines, written to understand the pattern.

## What I would tell myself a year ago

If a correct outcome depends on a process staying alive to remember
something, the outcome is not correct yet. Store the fact, compare on every
pass, and treat every timer as a hint.

Code: https://github.com/pranjalbareth/runway
