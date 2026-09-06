// Package harness starts a live Task API and gives a test the verbs to drive
// it.
//
// It exists so that an end-to-end test is a story rather than a fixture. Every
// helper here either does something to the system or asserts something about
// it, and reports its own failure — so a scenario reads as a sequence of
// events and the assertions do not bury it:
//
//	sys := harness.Start(t)
//
//	created := sys.API.Create("buy milk", 0)
//	sys.Webhook.AwaitEvent(created.ID, pgrepo.EventCreated)
//	sys.Jobs.AwaitSettled(1).AllSucceeded()
//
// # What Start brings up
//
//	                      the test binary
//	                            │
//	   ┌────────────────────────┼──────────────────────────┐
//	   │                        │                          │
//	Webhook                    API                        Jobs / Tasks
//	(a real socket)        (the contract)             (rows, asserted directly)
//	   │                        │                          │
//	   │ POST /hook             │ POST /tasks              │ SELECT
//	   │                        ▼                          │
//	   │                 ┌─────────────┐                   │
//	   │                 │  taskapi    │──── INSERT ───────┤
//	   │                 │    serve    │   task + job,     │
//	   │                 └─────────────┘   one transaction │
//	   │                                                   ▼
//	   │                 ┌─────────────┐            ┌────────────┐
//	   └─────────────────│  taskapi    │─── claim ──│  Postgres  │
//	                     │   worker    │            │ (private,  │
//	                     └─────────────┘            │  per test) │
//	                                                └────────────┘
//
// Both processes are the binary that ships, started the way the container
// starts it and configured entirely through TASKAPI_*. Neither knows the other
// exists. That is the point: a job genuinely crosses a process boundary, and
// kill(2) is available — which is the only honest way to test a lease.
//
// # Three decisions that make it deterministic
//
// Every listener binds port 0 and the process logs the port it got; the
// harness reads it back out of the log stream. Picking a "free" port in the
// test leaves a window for anything else on the machine to take it, which is a
// class of flake that only appears in CI.
//
// The webhook receiver is a Go object, not a service. Webhook.Hold parks every
// delivery inside its handler, so a crash lands while a request is genuinely
// in flight rather than whenever the timing works out.
//
// Nothing sleeps. Every wait is on a delivery, a log record or a row, with a
// deadline that reports what it last saw.
package harness
