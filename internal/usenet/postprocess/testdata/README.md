# Test fixtures

## RAR files

`rar5-psw.rar`, `rar5-subdirs.rar` and `rar5-symlink-unix.rar` come from the
[rarfile](https://github.com/markokr/rarfile) test suite (`test/files/`),
Copyright (c) 2005-2026 Marko Kreen, ISC licensed — see `LICENSE.rarfile`,
redistributed here under its terms.

They are vendored rather than generated because creating a RAR archive requires
the proprietary `rar` compressor, which is not installable in CI (only `unrar`,
the free decompressor, is). Vendoring keeps the tests hermetic: no network, no
non-free tool.

Chosen for what each one exercises:

| File | Exercises |
|---|---|
| `rar5-subdirs.rar` | nested directories, unicode and spaces in member names |
| `rar5-symlink-unix.rar` | symlink members — must be REFUSED by `safeWriter` |
| `rar5-psw.rar` | AES-encrypted bodies (password `password`), plus the wrong-password counterfactual |

## Everything else

7z and zip fixtures are generated inside the tests (`archive/zip` for zip,
handcrafted bytes for the malicious cases). Nothing else is vendored: anything
that can be built in-process is, so the fixture set stays as small as possible.
