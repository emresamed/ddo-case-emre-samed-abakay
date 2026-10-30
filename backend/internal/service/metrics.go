package service

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	holdsCreated = promauto.NewCounter(prometheus.CounterOpts{Name: "sahnebilet_holds_created_total", Help: "Seat holds created."})
	holdsRejected = promauto.NewCounter(prometheus.CounterOpts{Name: "sahnebilet_holds_rejected_total", Help: "Seat hold attempts rejected (taken, rule, busy)."})
	holdsExpired = promauto.NewCounter(prometheus.CounterOpts{Name: "sahnebilet_hold_seats_expired_total", Help: "Seats released by the expiry sweeper."})
	ticketsSold  = promauto.NewCounterVec(prometheus.CounterOpts{Name: "sahnebilet_tickets_sold_total", Help: "Tickets sold."}, []string{"channel"})
	ticketsRefunded = promauto.NewCounter(prometheus.CounterOpts{Name: "sahnebilet_tickets_refunded_total", Help: "Tickets refunded."})
	gateScans    = promauto.NewCounterVec(prometheus.CounterOpts{Name: "sahnebilet_gate_scans_total", Help: "Gate scans by result."}, []string{"result"})
)
