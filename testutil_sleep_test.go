package main

import "time"

func timeSleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
