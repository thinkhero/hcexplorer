# Command line app `rebuilddb2`

The `rebuilddb2` app is used for maintenance of hcexplorer's `hcpg` database (a.k.a. DB v2) that uses PostgreSQL to store a nearly complete record of the Hcd blockchain data.

**IMPORTANT**: When performing a bulk data import (e.g. full chain scan from genesis block), be sure to configure PostgreSQL appropriately.  Please see [postgresql-tuning.conf](../../db/hcpg/postgresql-tuning.conf) for tips.

## Installation

Be able to build hcexplorer (see [../../README.md](../../README.md#build-from-source)). In short:

* Install `dep`, the dependency management tool

      go get -u -v github.com/golang/dep/cmd/dep

* Clone the hcexplorer repository

      git clone https://github.com/HcashOrg/hcexplorer $GOPATH/src/github.com/HcashOrg/hcexplorer

* Populate vendor folder with `dep ensure`

      cd $GOPATH/src/github.com/HcashOrg/hcexplorer
      dep ensure

* Build `rebuilddb2`

      # build rebuilddb2 executable in workspace:
      cd $GOPATH/src/github.com/HcashOrg/hcexplorer/cmd/rebuilddb2
      go build
      # or to install hcexplorer and other tools into $GOPATH/bin:
      go install ./cmd/rebuilddb2

## Usage

First edit rebuilddb2.conf, using sample-rebuilddb2.conf to start.  You will need to follow a typical PostgreSQL setup process, creating a new database/scheme and a new role that has permissions/owns that database.

A fresh rebuild of the database is accomplished via:

```
./rebuilddb2 -D  # drop any existing tables
./rebuilddb2 -u  # rebuild tables, and update (-u) address table from scratch
```

Running without `-u` is only appropriate when the tables are behind the network's current best block by **at most** a few thousand blocks.  Otherwise, run with `-u` to recreate the address table in a more efficient batch process.

Remember to update your PostgreSQL config (postgresql.conf) before *and after* bulk data imports. Namely, before normal hcexplorer operation, ensure that `fsync=true` and other setting are adjusted for efficient queries.

Use the `--help` flag for more information.

## Details

Rebuilding the hcexplorer tables from scratch involves the following steps:

* Connect to the PostgreSQL database using the settings in rebuilddb2.conf
* Create tables: "blocks", "transactions", "vins", "vouts", and "addresses".
* Starting from genesis block, process each block and store in tables.
* If `-u` is not specified, a relatively expensive process is used to keep the spending transaction information up-to-date in the "addresses" table.
* If `-u` is specified, updating this part of the "addresses" table is deferred until after all other tables are populated and indexes for faster queries.

## License

See [LICENSE](../../LICENSE) at the base of the hcexplorer repository.

