-- +goose Up
ALTER TABLE delivery_incidents
  CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
ALTER TABLE delivery_incident_items
  CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
ALTER TABLE delivery_incident_evidence
  CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
ALTER TABLE delivery_incident_history
  CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci,
  ADD KEY idx_delivery_incident_history_actor_action_created (actor_type,actor_id,action,created_at);

-- +goose Down
ALTER TABLE delivery_incident_history
  DROP INDEX idx_delivery_incident_history_actor_action_created;
