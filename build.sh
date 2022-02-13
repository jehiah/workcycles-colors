#!/bin/bash

# yaml2json = https://github.com/bronze1man/yaml2json

yaml2json <data/bikes.yaml | jq . > static/bikes.json
