#pragma once

char *uri_get_host(char *uri);
char *webkit_auth_window(char *auth_url, char *account_name);
void show_auth_failure_dialog(char *message);
