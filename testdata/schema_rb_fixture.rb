# This file is auto-generated from the current state of the database. Instead
# of editing this file, please use the migrations feature of Active Record to
# incrementally modify your database, and then regenerate this schema definition.
#
# Fixture sliced from the real foundation schema shape for parser tests.

ActiveRecord::Schema[8.1].define(version: 2026_08_11_140000) do
  # These are extensions that must be enabled in order to support this database
  enable_extension "pg_catalog.plpgsql"

  create_table "active_storage_attachments", force: :cascade do |t|
    t.bigint "blob_id", null: false
    t.datetime "created_at", null: false
    t.string "name", null: false
    t.bigint "record_id", null: false
    t.string "record_type", null: false
    t.index ["blob_id"], name: "index_active_storage_attachments_on_blob_id"
    t.index ["record_type", "record_id", "name", "blob_id"], name: "index_active_storage_attachments_uniqueness", unique: true
  end

  create_table "crm_companies", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "domain"
    t.string "industry"
    t.string "name", null: false
    t.bigint "organization_id", null: false
    t.string "phone"
    t.datetime "updated_at", null: false
    t.string "website"
    t.index ["organization_id", "domain"], name: "index_crm_companies_on_org_domain", unique: true, where: "((domain IS NOT NULL) AND (length(btrim((domain)::text)) > 0))"
    t.index ["organization_id", "name"], name: "index_crm_companies_on_organization_id_and_name"
    t.index ["organization_id"], name: "index_crm_companies_on_organization_id"
    t.check_constraint "length(btrim(name::text)) > 0", name: "crm_companies_name_present"
  end

  create_table "crm_contacts", force: :cascade do |t|
    t.bigint "company_id"
    t.datetime "created_at", null: false
    t.string "email"
    t.string "first_name", default: "", null: false
    t.string "last_name", default: "", null: false
    t.bigint "organization_id", null: false
    t.bigint "owner_id"
    t.string "phone"
    t.string "title"
    t.datetime "updated_at", null: false
    t.index ["company_id"], name: "index_crm_contacts_on_company_id"
    t.index ["organization_id", "email"], name: "index_crm_contacts_on_organization_id_and_email", where: "(email IS NOT NULL)"
    t.index ["organization_id", "last_name", "first_name"], name: "idx_on_organization_id_last_name_first_name_ac1191ba56"
    t.index ["organization_id", "owner_id"], name: "index_crm_contacts_on_organization_id_and_owner_id"
    t.index ["organization_id"], name: "index_crm_contacts_on_organization_id"
    t.index ["owner_id"], name: "index_crm_contacts_on_owner_id"
    t.check_constraint "length(btrim(first_name::text)) > 0 OR length(btrim(last_name::text)) > 0 OR length(btrim(COALESCE(email, ''::character varying)::text)) > 0", name: "crm_contacts_identity_present"
  end

  create_table "storefront_orders", force: :cascade do |t|
    t.string "acceptance_ip"
    t.string "acceptance_user_agent"
    t.datetime "canceled_at"
    t.string "checkout_key_digest", null: false
    t.datetime "checkout_started_at"
    t.datetime "created_at", null: false
    t.string "currency", limit: 3, null: false
    t.string "email", null: false
    t.datetime "fulfilled_at"
    t.datetime "inventory_released_at"
    t.datetime "legal_accepted_at", null: false
    t.datetime "paid_at"
    t.string "privacy_version", null: false
    t.string "provider_payment_id"
    t.string "public_reference", null: false
    t.datetime "receipt_queued_at"
    t.datetime "receipt_sent_at"
    t.datetime "refunded_at"
    t.datetime "reservation_expires_at", null: false
    t.boolean "simulated", default: false, null: false
    t.string "state", default: "pending", null: false
    t.string "stripe_session_id"
    t.bigint "subtotal_cents", null: false
    t.string "terms_version", null: false
    t.bigint "total_cents", null: false
    t.datetime "updated_at", null: false
    t.bigint "user_id"
    t.index ["checkout_key_digest"], name: "index_storefront_orders_on_checkout_key_digest", unique: true
    t.index ["provider_payment_id"], name: "index_storefront_orders_on_provider_payment_id", unique: true, where: "(provider_payment_id IS NOT NULL)"
    t.index ["public_reference"], name: "index_storefront_orders_on_public_reference", unique: true
    t.index ["state", "created_at"], name: "index_storefront_orders_on_state_and_created_at"
    t.index ["state", "reservation_expires_at"], name: "index_storefront_orders_on_state_and_reservation_expires_at"
    t.index ["stripe_session_id"], name: "index_storefront_orders_on_stripe_session_id", unique: true, where: "(stripe_session_id IS NOT NULL)"
    t.index ["user_id", "created_at"], name: "index_storefront_orders_on_user_id_and_created_at"
    t.index ["user_id"], name: "index_storefront_orders_on_user_id"
    t.check_constraint "currency::text = upper(currency::text) AND currency::text ~ '^[A-Z]{3}$'::text", name: "storefront_orders_currency_format"
    t.check_constraint "length(btrim(email::text)) > 0", name: "storefront_orders_email_present"
    t.check_constraint "state::text = ANY (ARRAY['pending'::character varying::text, 'paid'::character varying::text, 'fulfilled'::character varying::text, 'canceled'::character varying::text, 'refunded'::character varying::text])", name: "storefront_orders_state_allowed"
    t.check_constraint "subtotal_cents = total_cents", name: "storefront_orders_total_matches_subtotal"
    t.check_constraint "subtotal_cents >= 0 AND total_cents >= 0", name: "storefront_orders_totals_nonnegative"
  end

  create_table "storefront_line_items", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "currency", limit: 3, null: false
    t.bigint "line_total_cents", null: false
    t.string "name", null: false
    t.bigint "order_id", null: false
    t.bigint "product_id"
    t.integer "quantity", null: false
    t.string "sku", null: false
    t.bigint "unit_price_cents", null: false
    t.datetime "updated_at", null: false
    t.index ["order_id", "product_id"], name: "index_storefront_line_items_on_order_id_and_product_id"
    t.index ["order_id"], name: "index_storefront_line_items_on_order_id"
    t.index ["product_id"], name: "index_storefront_line_items_on_product_id"
    t.check_constraint "currency::text = upper(currency::text) AND currency::text ~ '^[A-Z]{3}$'::text", name: "storefront_line_items_currency_format"
    t.check_constraint "length(btrim(name::text)) > 0 AND length(btrim(sku::text)) > 0", name: "storefront_line_items_snapshot_present"
    t.check_constraint "line_total_cents = (unit_price_cents * quantity)", name: "storefront_line_items_total_matches"
    t.check_constraint "quantity >= 1 AND quantity <= 10", name: "storefront_line_items_quantity_range"
    t.check_constraint "unit_price_cents >= 0 AND line_total_cents >= 0", name: "storefront_line_items_prices_nonnegative"
  end

  create_table "users", force: :cascade do |t|
    t.boolean "admin", default: false, null: false
    t.datetime "confirmation_sent_at"
    t.string "confirmation_token"
    t.datetime "confirmed_at"
    t.datetime "created_at", null: false
    t.string "email", default: "", null: false
    t.string "encrypted_password", default: "", null: false
    t.integer "failed_attempts", default: 0, null: false
    t.datetime "locked_at"
    t.datetime "remember_created_at"
    t.datetime "reset_password_sent_at"
    t.string "reset_password_token"
    t.string "unconfirmed_email"
    t.string "unlock_token"
    t.datetime "updated_at", null: false
    t.index ["confirmation_token"], name: "index_users_on_confirmation_token", unique: true
    t.index ["email"], name: "index_users_on_email", unique: true
    t.index ["reset_password_token"], name: "index_users_on_reset_password_token", unique: true
    t.index ["unlock_token"], name: "index_users_on_unlock_token", unique: true
  end

  add_foreign_key "active_storage_attachments", "active_storage_blobs", column: "blob_id"
  add_foreign_key "crm_companies", "organizations_organizations", column: "organization_id", on_delete: :cascade
  add_foreign_key "crm_contacts", "crm_companies", column: "company_id", on_delete: :nullify
  add_foreign_key "crm_contacts", "organizations_organizations", column: "organization_id", on_delete: :cascade
  add_foreign_key "crm_contacts", "users", column: "owner_id", on_delete: :nullify
  add_foreign_key "storefront_line_items", "storefront_orders", column: "order_id", on_delete: :cascade
  add_foreign_key "storefront_line_items", "storefront_products", column: "product_id", on_delete: :nullify
  add_foreign_key "storefront_orders", "users", on_delete: :nullify
end
