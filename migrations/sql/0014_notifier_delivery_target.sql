-- 0014_notifier_delivery_target: the recipient of a delivery (spec 15.4).
-- An alert action names its target (slack("channel") overrides the
-- channel's default_channel, email("addr") is the recipient), which
-- spec 10.6's deliveries table has no column for.
ALTER TABLE notifier.deliveries ADD COLUMN target text NOT NULL DEFAULT '';
