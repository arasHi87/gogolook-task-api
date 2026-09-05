// The sink is its own module. It is a demo dependency, not part of the
// service, and keeping it separate stops it appearing in the service's
// dependency graph, its vet run or its coverage.
module webhook-sink

go 1.26
