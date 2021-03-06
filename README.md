# Find My Device (fmd)

[![Build Status: Travis](https://travis-ci.org/mozilla-services/FindMyDevice.svg?branch=dev)](https://travis-ci.org/mozilla-services/FindMyDevice)

This server is still under development.

**Note:** The most stable version of this server is the *master* branch. "dev"
is marked as default to prevent accidental pushes to the master branch.

[Server API Reference Documentation](https://wiki.mozilla.org/Services/WheresMyFox#Server_API_Reference.2FDocumentation)

## Prerequisites:

You will need:

- A [Postgres](http://www.postgresql.org/) database
- [golang](http://golang.org/) 1.3 or greater
- [Node.js](http://nodejs.org/) 0.10.x or greater and [npm](http://npmjs.org/)

## How to install:

1. Run `./install.bash` (will generate "./FindMyDevice")
  - For Production level installs, you will need to also run:
    `grunt build` in the ./static/ library. This will create a
    ./static/dist/ directory containing prebuilt items.
2. Copy [config-example.ini](config-sample.ini) to config.ini
3. Modify config.ini to reflect your system and preferences.

## Running:

`GOPATH` needs to be set to the root install directory. e.g.

```sh
./runserver.bash
```
