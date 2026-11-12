# Workspace directory name and description search

Goal: Extend GET /v1/space/{space_id}/projects keyword matching to name OR description before pagination, using the same predicate for rows and X-Total-Count.

Base: GitHub Mininglamp-OSS/octo-server main c97b3ab922e90e450ea83d0b6852daf30b5f9559. Pull request target: GitHub upstream main.

Load-bearing behavior: tenant and active membership isolation, archive filtering, literal LIKE escaping, consistent list/count predicate.

Out of scope: client changes, new search parameters, schema changes, lifecycle behavior and deployment.

Acceptance: description-only and name-only matches; stable cross-page results and total; both-field matches counted once; archived filters; inactive/removing memberships and other spaces excluded; percent, underscore and exclamation searched literally.
