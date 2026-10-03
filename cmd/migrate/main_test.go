package main

import "testing"

func TestMigrateURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "postgres scheme",
			in:   "postgres://user:pass@host:5432/db?sslmode=require",
			want: "pgx5://user:pass@host:5432/db?sslmode=require",
		},
		{
			name: "postgresql scheme as issued by neon",
			in:   "postgresql://user:pass@host/db?sslmode=require&channel_binding=require",
			want: "pgx5://user:pass@host/db?sslmode=require&channel_binding=require",
		},
		{
			name: "already prefixed",
			in:   "pgx5://user:pass@host/db",
			want: "pgx5://user:pass@host/db",
		},
		{
			name: "unix socket left alone",
			in:   "unix:///var/run/postgresql",
			want: "unix:///var/run/postgresql",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := migrateURL(tc.in); got != tc.want {
				t.Errorf("migrateURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMigrationName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "already valid", in: "add_campaigns", want: "add_campaigns"},
		{name: "spaces become underscores", in: "add campaigns", want: "add_campaigns"},
		{name: "mixed case lowered", in: "AddCampaigns", want: "addcampaigns"},
		{name: "dashes become underscores", in: "add-campaigns", want: "add_campaigns"},
		{name: "path separators stripped", in: "../../etc/passwd", want: "etc_passwd"},
		{name: "quotes stripped", in: `add"; DROP TABLE users; --`, want: "add_drop_table_users"},
		{name: "trimmed", in: "  add_campaigns  ", want: "add_campaigns"},
		{name: "empty", in: "", wantErr: true},
		{name: "only punctuation", in: "///", wantErr: true},
		{name: "only separators", in: "---", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := migrationName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("migrationName(%q) error = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("migrationName(%q) error = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("migrationName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
