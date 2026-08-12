# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- First release of the Go client: list a project's generated tools, invoke one,
  and invoke one as a named end customer with `AsClient`.
- `StaticToken` for project API keys and `RefreshingToken` for a user's rotating
  session, behind a single `TokenSource` interface.
