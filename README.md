# kaleidoscope

A self-contained mirror of mirrors for Arch Linux.

Automatically redirects clients to an active and up-to-date Arch HTTP mirror
either globally or within a country of choice.


## Installation

    go get github.com/JoeHazzers/kaleidoscope

## Running

Simply run the executable, using the `-h` argument for usage information. By
default, the application pulls data from the Arch [mirror status site][] hourly.

Also, only complete mirrors and those supporting HTTP are considered for
redirection.


## TODO

* HTTPS
* Stats
* Homepage
* JSON output
* Intelligent update interval detection
* Geolocation
