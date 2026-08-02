// Package integration holds tests that need a real PostgreSQL, started in a
// container. They are behind the `integration` build tag, so `make test` stays
// fast and works without Docker, while `make test-integration` runs everything.
//
// The heavy use of jsonb means a mocked database would test nothing worth
// testing (DESIGN.md §9.1), so the schema and anything built on it is verified
// against the real server.
package integration
