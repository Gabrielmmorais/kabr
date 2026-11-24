# Samples

- `datasource.yaml`: a DataSource referencing the Secret `pagila-reader` by
  name. The sample holds no credentials; create the Secret yourself.
- DomainPlans are not hand-written: approve a plan file with
  `kabr plan approve`, then render its custom resource with
  `kabr plan manifest examples/pagila.plan.yaml --datasource pagila-db --namespace kabr-demo`.
  The operator serves nothing unless the approval digest still matches the spec.
