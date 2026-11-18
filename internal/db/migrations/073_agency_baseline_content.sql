-- 073_agency_baseline_content.sql — baseline catalog for a fresh install.
--
-- The agency tables ship empty; without packages the public pricing section
-- and authenticated checkout have nothing to offer. These rows match the
-- original DFD seed and can be edited from the admin Packages screen. Inserts
-- are idempotent and never overwrite an existing catalog.
INSERT INTO packages (name, slug, price, features, sort_order, is_active)
VALUES
  ('Landing Page', 'landing-page', 1500000,
   '["Responsive","SEO Baseline","1 Section","Hosting 1 year"]'::jsonb, 1, TRUE),
  ('Business Pro', 'business-pro', 3500000,
   '["Multi-page","Contact Form","Custom CMS","Hosting 1 year"]'::jsonb, 2, TRUE),
  ('E-Commerce AI', 'ecommerce-ai', 7500000,
   '["Payment Gateway","Inventory","AI Copywriter","Hosting 1 year"]'::jsonb, 3, TRUE),
  ('Custom Elite Solutions', 'custom', 0,
   '["Custom architecture","Dedicated squad","Priority support","Flexible scope"]'::jsonb, 4, TRUE)
ON CONFLICT (slug) DO NOTHING;

INSERT INTO global_settings (whatsapp_number, email_contact, office_address, is_maintenance_mode)
SELECT '62895324350359', 'hello@dfdagency.com', 'Jakarta, Indonesia', FALSE
WHERE NOT EXISTS (SELECT 1 FROM global_settings);
