#!/bin/sh
# Sourced by the installer/controller; restore the original root mount mode.
firmware_ro=false
firmware_writable() {
 if awk '$2 == "/" && $4 ~ /(^|,)ro(,|$)/ {found=1} END {exit !found}' /proc/mounts; then
  mount -o remount,rw /
  firmware_ro=true
 fi
}
firmware_restore() {
 sync
 if [ "$firmware_ro" = true ]; then
  mount -o remount,ro / || return 1
  firmware_ro=false
 fi
}
