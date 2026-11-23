set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

import 'just/build.just'
import 'just/dev.just'
import 'just/test.just'
import 'just/demo.just'

default:
    @just --list

import 'just/distribution.just'
