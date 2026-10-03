## Summary

Describe the problem, behavior change, and why this approach was chosen.

## Compatibility and risk

- Protocol/API/config/schema changes:
- Security and privacy impact:
- Multi-instance/concurrency impact:
- Migration and rollback:

## Validation

List commands actually run, database versions, and every skipped or unverified check. Never point tests at production.

- [ ] `make lint`
- [ ] `make test`
- [ ] `make race`
- [ ] `make test-pg` against a dedicated database when storage, Provider, callback, RBAC, or migrations changed
- [ ] `make test-deploy-scripts` when deployment behavior changed
- [ ] Documentation and CHANGELOG updated
- [ ] No credentials, private URLs, customer data, or production request/response bodies included

## Evidence

Provide sanitized logs, test names, screenshots, or benchmark results needed for review.
