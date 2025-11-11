package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
)

// Parameters which define thresholds to trigger scalling up and scale factor
var (
	replicaUpperThreshold        = 0.7
	replicaLowerThreshold        = 0.3
	scaleFactor                  = 2.
	minReplicasCount             = int32(2)
	maxReplicasCount             = int32(0)
	fixedReplicasOverrideEnabled bool
)

// Get all parameters from ENV variables
// Extra check is performed not to fall into the infinite loop:
// replicaDownTrigger < replicaUpperThreshold/scaleFactor
func getEnvVariables() {
	if ep := os.Getenv("SCALE_FACTOR"); ep != "" {
		factor, err := strconv.ParseFloat(ep, 64)
		if err != nil {
			slog.Error("Could not parse environment SCALE_FACTOR variable", "error", err)
			os.Exit(1)
		} else if factor > 1 {
			scaleFactor = factor
		}
	}

	if ep := os.Getenv("REPLICA_UPSCALE_TRIGGER"); ep != "" {
		replicaUpTrigger, err := strconv.ParseFloat(ep, 64)
		if err != nil {
			slog.Error("Could not parse environment REPLICA_UPSCALE_TRIGGER variable", "error", err)
			os.Exit(1)
		} else if replicaUpTrigger > 0.1 {
			replicaUpperThreshold = replicaUpTrigger
		}
	}

	if ep := os.Getenv("REPLICA_DOWNSCALE_TRIGGER"); ep != "" {
		replicaDownTrigger, err := strconv.ParseFloat(ep, 64)
		if err != nil {
			slog.Error("Could not parse environment REPLICA_DOWNSCALE_TRIGGER variable", "error", err)
			os.Exit(1)
		} else if replicaDownTrigger < replicaUpperThreshold/scaleFactor {
			replicaLowerThreshold = replicaDownTrigger
		}
	}

	if ep := os.Getenv("MIN_REPLICAS_COUNT"); ep != "" {
		minReplicas, err := strconv.ParseInt(ep, 10, 32)
		if err != nil {
			slog.Error("Could not parse environment MIN_REPLICAS_COUNT variable", "error", err)
			os.Exit(1)
		} else if minReplicas >= 0 {
			minReplicasCount = int32(minReplicas)
		}
	}

	if ep := os.Getenv("MAX_REPLICAS_COUNT"); ep != "" {
		maxReplicas, err := strconv.ParseInt(ep, 10, 32)
		if err != nil {
			slog.Error("Could not parse environment MAX_REPLICAS_COUNT variable", "error", err)
			os.Exit(1)
		} else if maxReplicas >= 0 {
			maxReplicasCount = int32(maxReplicas)
		}
	}

	if ep := os.Getenv("FIXED_REPLICAS"); ep != "" {
		if ep == "true" {
			fixedReplicasOverrideEnabled = true
			slog.Info("FIXED_REPLICAS override is enabled")
		} else {
			fixedReplicasOverrideEnabled = false
			slog.Info("FIXED_REPLICAS override is disabled")
		}
	}

	// No need to read ROOMS_PER_REPLICA; we derive from room.Capacity

	// Extra check: In order not to fall into infinite loop
	// we change down scale trigger, so that after we scale up
	// fleet does not immediately scales down and vice versa
	if replicaLowerThreshold >= replicaUpperThreshold/scaleFactor {
		replicaLowerThreshold = replicaUpperThreshold / (scaleFactor + 1)
	}

	if maxReplicasCount > 0 && minReplicasCount > maxReplicasCount {
		slog.Info("MIN_REPLICAS_COUNT exceeds MAX_REPLICAS_COUNT; adjusting min to max", "min", minReplicasCount, "max", maxReplicasCount)
		minReplicasCount = maxReplicasCount
	}
}

// Main will set up an http server and three endpoints
func init() {
	// Configure slog to emit JSON to stdout at Info level.
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))
}

// Main will set up an http server and three endpoints
func main() {
	port := flag.String("port", "8000", "The port to listen to TCP requests")
	flag.Parse()
	if ep := os.Getenv("PORT"); ep != "" {
		port = &ep
	}
	getEnvVariables()
	// Run the HTTP server using the bound certificate and key for TLS
	// Serve 200 status on /health for k8s health checks
	http.HandleFunc("/health", handleHealth)

	// Return the target replica count which is used by Webhook fleet autoscaling policy
	http.HandleFunc("/scale", handleAutoscale)

	_, err := os.Stat("/home/service/certs/tls.crt")
	if err == nil {
		slog.Info("Starting HTTPS server", "port", *port)
		if err := http.ListenAndServeTLS(":"+*port, "/home/service/certs/tls.crt", "/home/service/certs/tls.key", nil); err != nil {
			slog.Error("HTTPS server failed to run", "error", err)
		}
	} else {
		slog.Info("Starting HTTP server", "port", *port)
		if err := http.ListenAndServe(":"+*port, nil); err != nil {
			slog.Error("HTTP server failed to run", "error", err, "port", *port)
		}
	}
}

// Let /health return Healthy and status code 200
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "Healthy")

}

// handleAutoscale is a handler function which return the replica count
// based on received status of the fleet
func handleAutoscale(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		http.Error(w, "Empty request", http.StatusInternalServerError)
		return
	}

	var faReq autoscalingv1.FleetAutoscaleReview
	if err := json.NewDecoder(r.Body).Decode(&faReq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	faResp := autoscalingv1.FleetAutoscaleResponse{
		Scale:    false,
		Replicas: faReq.Request.Status.Replicas,
		UID:      faReq.Request.UID,
	}

	if fixedReplicasOverrideEnabled {
		if faReq.Request.Annotations != nil {
			if value, ok := faReq.Request.Annotations["fixedReplicas"]; ok {
				replicas, err := strconv.Atoi(value)
				if err != nil {
					slog.Error("Invalid fixedReplicas value in annotations", "error", err)
					http.Error(w, "Invalid fixedReplicas value. Must be an integer.", http.StatusBadRequest)
					return
				}

				if replicas < 0 {
					slog.Error("fixedReplicas value cannot be negative")
					http.Error(w, "Invalid fixedReplicas value. Must be >= 0.", http.StatusBadRequest)
					return
				}

				faResp.Scale = true
				faResp.Replicas = int32(replicas)
			}
		}
	} else if faReq.Request.Status.Replicas != 0 {
		// If FleetStatus exposes a "room" counter, derive replicas from it.
		// FleetStatus.Counters["room"].Capacity is aggregated across the fleet.
		// capacityPerReplica = room.Capacity / currentReplicas
		// desiredReplicas = ceil(room.Count / capacityPerReplica)
		if faReq.Request.Status.Counters != nil {
			if room, ok := faReq.Request.Status.Counters["rooms"]; ok {
				// room.Count is expected to be an int64 aggregate across the fleet
				// room.Capacity is aggregated capacity across the fleet
				if room.Capacity > 0 && faReq.Request.Status.Replicas > 0 {
					current := faReq.Request.Status.Replicas
					capPerReplica := 5.0 // fixed value for now
					// Base target needed to cover rooms with current per-replica capacity
					desired := int32(math.Ceil(float64(room.Count) / capPerReplica))
					// Clamp base desired to min/max bounds
					if desired < minReplicasCount {
						desired = minReplicasCount
					}
					if maxReplicasCount > 0 && desired > maxReplicasCount {
						desired = maxReplicasCount
					}
					slog.Info("Calculated capacityPerReplica", "capacityPerReplica", capPerReplica, ", desired", desired, ", current", current, ", min", minReplicasCount, ", max", maxReplicasCount)
					// ignore a threshold
					next := int32(math.Ceil(float64(desired) * scaleFactor))
					// Final clamp to global bounds
					if next < minReplicasCount {
						next = minReplicasCount
					}
					if maxReplicasCount > 0 && next > maxReplicasCount {
						next = maxReplicasCount
					}
					// scale up only if needed
					if next != current {
						faResp.Scale = true
						faResp.Replicas = next
					}
					// Proceed to response
					w.Header().Set("Content-Type", "application/json")
					review := &autoscalingv1.FleetAutoscaleReview{
						Request:  faReq.Request,
						Response: &faResp,
					}
					_ = json.NewEncoder(w).Encode(review)
					return
				}
			}
		}
		allocatedPercent := float64(faReq.Request.Status.AllocatedReplicas) / float64(faReq.Request.Status.Replicas)
		if allocatedPercent > replicaUpperThreshold {
			// After scaling we would have percentage of 0.7/2 = 0.35 > replicaLowerThreshold
			// So we won't scale down immediately after scale up
			currentReplicas := float64(faReq.Request.Status.Replicas)
			faResp.Scale = true
			next := int32(math.Ceil(currentReplicas * scaleFactor))
			if maxReplicasCount > 0 && next > maxReplicasCount {
				next = maxReplicasCount
			}
			faResp.Replicas = next
		} else if allocatedPercent < replicaLowerThreshold && faReq.Request.Status.Replicas > minReplicasCount {
			faResp.Scale = true
			faResp.Replicas = int32(math.Ceil(float64(faReq.Request.Status.Replicas) / scaleFactor))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	review := &autoscalingv1.FleetAutoscaleReview{
		Request:  faReq.Request,
		Response: &faResp,
	}
	// Enforce MAX_REPLICAS_COUNT for fixed override as well
	if maxReplicasCount > 0 && faResp.Replicas > maxReplicasCount {
		faResp.Scale = true
		faResp.Replicas = maxReplicasCount
	}

	_ = json.NewEncoder(w).Encode(review)
}
