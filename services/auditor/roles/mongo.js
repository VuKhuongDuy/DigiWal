// history-service MongoDB: a user with the built-in `read` role on the history database only (A2).
// DEV ONLY (fixed password). Run with: mongosh -u custody -p custody --authenticationDatabase admin roles/mongo.js
const h = db.getSiblingDB("history");
if (!h.getUser("auditor_ro")) {
  h.createUser({ user: "auditor_ro", pwd: "auditor_ro", roles: [{ role: "read", db: "history" }] });
}
